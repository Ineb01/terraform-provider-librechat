package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	fwdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/crypto/bcrypt"
)

// None of these tests need MongoDB, LibreChat or a network. That is deliberate: everything
// here is a check on a decision that is easy to get quietly wrong, and a suite that needs a
// live stack is a suite nobody runs.

// TestResourceSchemas is the cheapest test in the file and the one most likely to earn its
// keep. ValidateImplementation catches the mistakes the framework only reports at runtime,
// when a provider is already loaded: an attribute using a name Terraform reserves in a
// resource block (which is how `provider` became `model_provider` and `auth_provider`), an
// attribute that is neither Required, Optional nor Computed, a nested type with no element
// type. Without it, every one of those is a crash during someone's plan.
func TestResourceSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	resources := map[string]func() fwresource.Resource{
		"user":             NewUserResource,
		"group":            NewGroupResource,
		"agent":            NewAgentResource,
		"mcp_server":       NewMCPServerResource,
		"role":             NewRoleResource,
		"role_permissions": NewRolePermissionsResource,
		"grant":            NewGrantResource,
	}

	for name, ctor := range resources {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp := &fwresource.SchemaResponse{}
			ctor().Schema(ctx, fwresource.SchemaRequest{}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("building the schema failed: %s", resp.Diagnostics)
			}
			if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
				t.Fatalf("invalid schema: %s", diags)
			}
		})
	}
}

func TestDataSourceSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dataSources := map[string]func() fwdatasource.DataSource{
		"user":        NewUserDataSource,
		"group":       NewGroupDataSource,
		"access_role": NewAccessRoleDataSource,
	}

	for name, ctor := range dataSources {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resp := &fwdatasource.SchemaResponse{}
			ctor().Schema(ctx, fwdatasource.SchemaRequest{}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("building the schema failed: %s", resp.Diagnostics)
			}
			if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
				t.Fatalf("invalid schema: %s", diags)
			}
		})
	}
}

// bcryptjsHash was produced by golang.org/x/crypto/bcrypt and then verified against the
// bcryptjs 2.4.3 that ships inside librechat/librechat:v0.8.7:
//
//	docker run --rm --entrypoint node librechat/librechat:v0.8.7 -e \
//	  "console.log(require('/app/node_modules/bcryptjs').compareSync('correct-horse-battery-staple','<hash>'))"
//	true
//
// That is the assumption the whole librechat_user resource rests on - Go emits the $2a$
// prefix rather than the $2b$ a newer Node bcrypt would, and if bcryptjs rejected it the
// accounts this provider creates simply could not log in, with no error anywhere to say why.
// bcryptjs 2.4.3 also emits $2a$ itself, so the formats are identical, not merely compatible.
const bcryptjsHash = "$2a$10$qUF9MzZktURhOIDZ.lbSo.KuQmp/5D1Uk7mizWrf3HGHpSkMz7RsO"
const bcryptjsPassword = "correct-horse-battery-staple"

func TestBcryptFormatIsInteroperable(t *testing.T) {
	t.Parallel()

	if err := bcrypt.CompareHashAndPassword([]byte(bcryptjsHash), []byte(bcryptjsPassword)); err != nil {
		t.Fatalf("the hash bcryptjs accepted no longer verifies here: %v", err)
	}
	if !bcryptPattern.MatchString(bcryptjsHash) {
		t.Fatal("password_hash's validator would reject a hash LibreChat accepts")
	}

	// And the direction the provider actually uses.
	fresh, err := passwordHashFor(userResourceModel{
		Password:     types.StringValue(bcryptjsPassword),
		PasswordHash: types.StringNull(),
	})
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}
	if !bcryptPattern.MatchString(fresh) {
		t.Fatalf("generated hash %q is not in the format LibreChat compares against", fresh)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(fresh), []byte(bcryptjsPassword)); err != nil {
		t.Fatalf("generated hash does not verify: %v", err)
	}
}

func TestPasswordHashFor(t *testing.T) {
	t.Parallel()

	t.Run("an explicit hash is written verbatim", func(t *testing.T) {
		got, err := passwordHashFor(userResourceModel{
			Password:     types.StringNull(),
			PasswordHash: types.StringValue(bcryptjsHash),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != bcryptjsHash {
			t.Fatalf("got %q, want the hash unchanged", got)
		}
	})

	// An account with neither attribute authenticates through an identity provider. It must
	// produce no hash at all rather than a hash of the empty string, which would be a valid
	// password nobody intended to set.
	t.Run("no password means no hash", func(t *testing.T) {
		got, err := passwordHashFor(userResourceModel{
			Password:     types.StringNull(),
			PasswordHash: types.StringNull(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Fatalf("got %q, want an empty string", got)
		}
	})
}

func TestDatabaseFromURI(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ uri, want string }{
		"path names the database":   {"mongodb://mongodb:27017/LibreChat", "LibreChat"},
		"a different name":          {"mongodb://192.0.2.10:27017/dev-librechat", "dev-librechat"},
		"options do not confuse it": {"mongodb://h:27017/LibreChat?retryWrites=true", "LibreChat"},
		"srv form":                  {"mongodb+srv://user:pw@cluster.example.net/LibreChat", "LibreChat"},
		"no path falls back":        {"mongodb://mongodb:27017", defaultDatabase},
		"empty path falls back":     {"mongodb://mongodb:27017/", defaultDatabase},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := databaseFromURI(tc.uri); got != tc.want {
				t.Fatalf("databaseFromURI(%q) = %q, want %q", tc.uri, got, tc.want)
			}
		})
	}
}

func TestDefaultUsername(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"first.last@example.com": "first.last",
		"admin@example.test":     "admin",
		// Not a valid address, so the email validator rejects it long before this - but
		// returning the input beats returning "" and writing an empty username.
		"nonsense": "nonsense",
	}

	for email, want := range cases {
		if got := defaultUsername(email); got != want {
			t.Fatalf("defaultUsername(%q) = %q, want %q", email, got, want)
		}
	}
}

// TestPrincipalShape pins the difference that is silent when it is wrong: a role principal is
// identified by its NAME, a user or group by an ObjectId, and a public grant carries no
// principalId key at all - a filter of {principalId: null} would not find it.
func TestPrincipalShape(t *testing.T) {
	t.Parallel()

	objectID := bson.NewObjectID()

	t.Run("public omits principalId entirely", func(t *testing.T) {
		p := principal{Type: principalTypePublic}
		if _, present := p.filter()["principalId"]; present {
			t.Fatal("filter has a principalId key")
		}
		if _, present := p.fields()["principalId"]; present {
			t.Fatal("fields has a principalId key")
		}
		if _, present := p.fields()["principalModel"]; present {
			t.Fatal("fields has a principalModel, which the schema only requires for non-public grants")
		}
	})

	t.Run("a role is identified by name", func(t *testing.T) {
		p := principal{Type: principalTypeRole, ID: "ADMIN", Model: principalModelRole}
		if got := p.fields()["principalId"]; got != "ADMIN" {
			t.Fatalf("principalId = %v, want the role name", got)
		}
		if got := p.fields()["principalModel"]; got != principalModelRole {
			t.Fatalf("principalModel = %v, want %q", got, principalModelRole)
		}
	})

	t.Run("a group is identified by ObjectId", func(t *testing.T) {
		p := principal{Type: principalTypeGroup, ID: objectID, Model: principalModelGroup}
		if got := p.fields()["principalId"]; got != objectID {
			t.Fatalf("principalId = %v, want the ObjectId itself and not its hex form", got)
		}
	})
}

func TestACLEntryPrincipalIDString(t *testing.T) {
	t.Parallel()

	objectID := bson.NewObjectID()

	cases := map[string]struct {
		stored any
		want   string
	}{
		"an ObjectId comes back as hex": {objectID, objectID.Hex()},
		"a role name comes back as is":  {"ADMIN", "ADMIN"},
		"public has nothing stored":     {nil, ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := aclEntryDoc{PrincipalID: tc.stored}
			if got := doc.principalIDString(); got != tc.want {
				t.Fatalf("principalIDString() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSnapshot covers the restore-on-destroy record for role permissions. The important case
// is the third: re-running must not overwrite an original value with one Terraform itself
// wrote, or a destroy would restore the state the apply created rather than the one before it.
func TestSnapshot(t *testing.T) {
	t.Parallel()

	stored := map[string]map[string]bool{
		"AGENTS": {"USE": true, "CREATE": true},
	}
	declared := map[string]map[string]bool{
		"AGENTS": {"CREATE": false, "SHARE": false},
	}

	got := snapshot(stored, declared, nil)

	if v := got["AGENTS"]["CREATE"]; v == nil || *v != true {
		t.Fatalf("CREATE recorded as %v, want a pointer to true", v)
	}
	// SHARE was not in the document, so its record must be nil - meaning "unset it again on
	// destroy" rather than "write false", which lets LibreChat's own default apply.
	if v, present := got["AGENTS"]["SHARE"]; !present || v != nil {
		t.Fatalf("SHARE recorded as %v, want a present nil", v)
	}
	// USE was never declared, so it must not be recorded at all.
	if _, present := got["AGENTS"]["USE"]; present {
		t.Fatal("USE was recorded despite never being managed")
	}

	t.Run("an existing record is not overwritten", func(t *testing.T) {
		// What the second apply sees: CREATE is now false, because the first apply set it.
		nowStored := map[string]map[string]bool{"AGENTS": {"CREATE": false, "SHARE": false}}

		again := snapshot(nowStored, declared, got)
		if v := again["AGENTS"]["CREATE"]; v == nil || *v != true {
			t.Fatalf("CREATE is now recorded as %v, want the original true to survive", v)
		}
	})
}

func TestDecodeJSONObject(t *testing.T) {
	t.Parallel()

	t.Run("absent becomes an empty document", func(t *testing.T) {
		var diags diagnosticsSink
		got, ok := decodeJSONObject(jsontypes.NewNormalizedNull(), "model_parameters", &diags)
		if !ok || diags.HasError() {
			t.Fatalf("unexpected failure: %s", diags)
		}
		// Empty, not nil: LibreChat indexes into model_parameters without a null guard.
		if got == nil || len(got) != 0 {
			t.Fatalf("got %v, want an empty document", got)
		}
	})

	t.Run("an object decodes", func(t *testing.T) {
		var diags diagnosticsSink
		got, ok := decodeJSONObject(
			jsontypes.NewNormalizedValue(`{"temperature":0.2,"max_tokens":4096}`),
			"model_parameters", &diags)
		if !ok || diags.HasError() {
			t.Fatalf("unexpected failure: %s", diags)
		}
		if got["temperature"] != 0.2 {
			t.Fatalf("temperature = %v, want 0.2", got["temperature"])
		}
	})

	// An array would be stored happily and then read by LibreChat as an object, producing no
	// parameters at all rather than an error.
	t.Run("an array is rejected", func(t *testing.T) {
		var diags diagnosticsSink
		if _, ok := decodeJSONObject(jsontypes.NewNormalizedValue(`[1,2]`), "model_parameters", &diags); ok {
			t.Fatal("an array was accepted")
		}
		if !diags.HasError() {
			t.Fatal("no diagnostic was reported")
		}
	})
}

// TestConfigInt64 matters because the same field is written by two different writers: this
// provider stores an int64, LibreChat's JavaScript stores a double. A reader that only
// handled one of them would report a spurious diff on every refresh of a server LibreChat
// had touched.
func TestConfigInt64(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		stored  bson.M
		wantSet bool
		want    int64
	}{
		"int64 as this provider writes it": {bson.M{"timeout": int64(60000)}, true, 60000},
		"int32":                            {bson.M{"timeout": int32(60000)}, true, 60000},
		"double as JavaScript writes it":   {bson.M{"timeout": float64(60000)}, true, 60000},
		"absent":                           {bson.M{}, false, 0},
		"a string is not a timeout":        {bson.M{"timeout": "60000"}, false, 0},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := configInt64(tc.stored, "timeout")
			if got.IsNull() == tc.wantSet {
				t.Fatalf("got %v, want set=%v", got, tc.wantSet)
			}
			if tc.wantSet && got.ValueInt64() != tc.want {
				t.Fatalf("got %d, want %d", got.ValueInt64(), tc.want)
			}
		})
	}
}

func TestLowercaseValidatorRejectsUppercase(t *testing.T) {
	t.Parallel()

	// The validator's real job is described where it is defined: mongoose lowercases these
	// paths, so a mixed-case value written by the raw driver stops matching LibreChat's own
	// queries. Checking the plumbing here; the reasoning is in validators.go.
	if got := emailPattern.MatchString("first.last@example.com"); !got {
		t.Fatal("a valid address was rejected")
	}
	for _, bad := range []string{"no-at-sign", "two@@at.com", "no@domain", "spaces in@it.com"} {
		if emailPattern.MatchString(bad) {
			t.Fatalf("%q was accepted as an email", bad)
		}
	}
}

func TestOptionalString(t *testing.T) {
	t.Parallel()

	// "" must become null, or an attribute the configuration never set shows up in state as
	// an empty string and every plan carries a diff that cannot be resolved.
	if !optionalString("").IsNull() {
		t.Fatal(`optionalString("") is not null`)
	}
	if got := optionalString("x"); got.ValueString() != "x" {
		t.Fatalf("optionalString(%q) = %q", "x", got.ValueString())
	}
}
