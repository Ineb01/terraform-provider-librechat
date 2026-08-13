package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"golang.org/x/crypto/bcrypt"
)

// LibreChat hashes with bcryptjs at 10 rounds. The cost is stored inside the hash and read
// back from it on comparison, so this value only has to be sane, not identical - but
// matching keeps login latency the same as for an account made through the sign-up form.
const bcryptCost = 10

// LibreChat's user schema sets `minlength: 8` on the password path. Mongoose does not
// enforce it on a document written from outside, so checking here is the only thing
// stopping a 3-character password that the application's own password-reset would later
// refuse to replace.
const minPasswordLength = 8

var (
	_ resource.Resource                = (*userResource)(nil)
	_ resource.ResourceWithConfigure   = (*userResource)(nil)
	_ resource.ResourceWithImportState = (*userResource)(nil)
)

type userResource struct {
	client *Client
}

func NewUserResource() resource.Resource { return &userResource{} }

type userResourceModel struct {
	ID            types.String `tfsdk:"id"`
	Email         types.String `tfsdk:"email"`
	Username      types.String `tfsdk:"username"`
	Name          types.String `tfsdk:"name"`
	Password      types.String `tfsdk:"password"`
	PasswordHash  types.String `tfsdk:"password_hash"`
	Role          types.String `tfsdk:"role"`
	EmailVerified types.Bool   `tfsdk:"email_verified"`
	AuthProvider  types.String `tfsdk:"auth_provider"`
	TermsAccepted types.Bool   `tfsdk:"terms_accepted"`
	CreatedAt     types.String `tfsdk:"created_at"`
	UpdatedAt     types.String `tfsdk:"updated_at"`
}

// userDoc mirrors only the fields this provider owns. Everything else on the document -
// refreshToken, totpSecret, favorites, skillStates, plugins - belongs to LibreChat and is
// never written on update, so a user who has since enabled two-factor auth does not lose it
// on the next apply.
type userDoc struct {
	ID            bson.ObjectID `bson:"_id"`
	Name          string        `bson:"name"`
	Username      string        `bson:"username"`
	Email         string        `bson:"email"`
	EmailVerified bool          `bson:"emailVerified"`
	Password      string        `bson:"password"`
	Provider      string        `bson:"provider"`
	Role          string        `bson:"role"`
	TermsAccepted bool          `bson:"termsAccepted"`
	CreatedAt     time.Time     `bson:"createdAt"`
	UpdatedAt     time.Time     `bson:"updatedAt"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A LibreChat account, written to the `users` collection with a " +
			"bcrypt password hash the application accepts - so unlike LibreChat's own " +
			"`npm run create-user`, the password is declarative and a change to it is an update " +
			"rather than a delete-and-recreate.\n\n" +
			"Destroying this removes the account and every ACL grant made **to** it. It does " +
			"**not** remove that account's conversations, messages or uploaded files: those live " +
			"in other collections and on disk, and deleting them is LibreChat's " +
			"`npm run delete-user`, not this provider's business.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The document's MongoDB ObjectId. This is the value `librechat_group.member_ids` and a `librechat_grant` for a user principal expect.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"email": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Login address. Must be lowercase.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						emailPattern,
						"must be a valid email address - LibreChat logs in by email and its schema rejects a malformed one",
					),
					lowercaseValidator{field: "email"},
				},
			},
			"username": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Display handle. Defaults to the local part of `email`. Must be " +
					"lowercase. Group membership and several LibreChat lookups go by username, so " +
					"changing it is a rename users will notice.",
				Validators: []validator.String{lowercaseValidator{field: "username"}},
				// Without this an Optional+Computed attribute the configuration leaves unset plans
				// as "(known after apply)" on every run, rather than keeping the value already in
				// state. It is noise on a plan that changes something else, and it is why an
				// imported account looked like it was about to be renamed.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Full name shown in the interface. Defaults to `username`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"password": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Plaintext password, hashed with bcrypt before it is written. " +
					"Conflicts with `password_hash`.\n\n" +
					"This value is kept in Terraform state, so state has to be treated as a secret " +
					"store - an encrypted remote backend is the minimum. To keep the plaintext out of " +
					"a committed `.tf` file, read it from whatever secret store the surrounding " +
					"configuration already uses, or supply `password_hash` instead.\n\n" +
					"Drift is detected properly: on refresh the stored hash is verified against this " +
					"value, and a password changed in the interface shows up as a diff.\n\n" +
					"Omit both this and `password_hash` for an account that authenticates through " +
					"an identity provider rather than a local password.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("password_hash")),
					stringvalidator.LengthAtLeast(minPasswordLength),
				},
			},
			"password_hash": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "A pre-computed bcrypt hash (`$2a$`/`$2b$`/`$2y$`), written " +
					"verbatim. Use this to keep a plaintext password out of state entirely. " +
					"Conflicts with `password`.",
				Validators: []validator.String{
					stringvalidator.ConflictsWith(path.MatchRoot("password")),
					stringvalidator.RegexMatches(
						bcryptPattern,
						"must be a bcrypt hash beginning $2a$, $2b$ or $2y$ - LibreChat compares with bcrypt and any other format can never match",
					),
				},
			},
			"role": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("USER"),
				MarkdownDescription: "Name of a document in the `roles` collection. This field **is** " +
					"admin access in LibreChat: `ADMIN` here is what unlocks the admin interface. " +
					"The role must already exist - see `librechat_role` and `librechat_role_permissions`.",
			},
			"email_verified": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Defaults to `true`, because an unverified account cannot log " +
					"in and nobody is going to click a confirmation link for an account created by " +
					"an apply. Set `false` only if a real verification mail is expected.",
			},
			"auth_provider": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("local"),
				MarkdownDescription: "LibreChat's `provider` field: which authentication source owns " +
					"this account. `local` means a password in this database. Named `auth_provider` " +
					"here only because `provider` is a reserved meta-argument in a resource block.",
			},
			"terms_accepted": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Whether the account has accepted the terms. LibreChat prompts on first login when false.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	username := plan.Username.ValueString()
	if plan.Username.IsUnknown() || username == "" {
		username = defaultUsername(plan.Email.ValueString())
	}
	name := plan.Name.ValueString()
	if plan.Name.IsUnknown() || name == "" {
		name = username
	}

	hash, diagErr := passwordHashFor(plan)
	if diagErr != nil {
		resp.Diagnostics.AddError("Cannot hash the password", diagErr.Error())
		return
	}

	if ok := r.checkRole(ctx, plan.Role.ValueString(), &resp.Diagnostics); !ok {
		return
	}

	now := time.Now().UTC()
	doc := bson.M{
		"name":          name,
		"username":      username,
		"email":         plan.Email.ValueString(),
		"emailVerified": plan.EmailVerified.ValueBool(),
		"provider":      plan.AuthProvider.ValueString(),
		"role":          plan.Role.ValueString(),
		"termsAccepted": plan.TermsAccepted.ValueBool(),
		// Written explicitly rather than left to mongoose's defaults, which do not apply to
		// a document inserted from outside the application. An absent plugins or
		// refreshToken array is read as null by LibreChat and breaks the account page.
		"plugins":         []string{},
		"refreshToken":    []any{},
		"favorites":       []any{},
		"skillStates":     bson.M{},
		"personalization": bson.M{"memories": true},
		"createdAt":       now,
		"updatedAt":       now,
		"__v":             0,
	}
	if hash != "" {
		doc["password"] = hash
	}

	res, err := r.client.users().InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			resp.Diagnostics.AddError(
				"That account already exists",
				fmt.Sprintf("A user with email %q is already in the database. LibreChat's unique "+
					"index is on email, so it cannot be created twice.\n\nIf it was created by hand "+
					"or by LibreChat's create-user script, adopt it instead of recreating it:\n\n"+
					"  tofu import <this resource address> %s\n\nOriginal error: %s",
					plan.Email.ValueString(), plan.Email.ValueString(), err),
			)
			return
		}
		resp.Diagnostics.AddError("Cannot create the user", err.Error())
		return
	}

	id, ok := res.InsertedID.(bson.ObjectID)
	if !ok {
		resp.Diagnostics.AddError("Unexpected inserted id",
			fmt.Sprintf("MongoDB returned %T instead of an ObjectId. This is a bug in the provider.", res.InsertedID))
		return
	}

	plan.ID = types.StringValue(id.Hex())
	plan.Username = types.StringValue(username)
	plan.Name = types.StringValue(name)
	plan.CreatedAt = types.StringValue(now.Format(time.RFC3339))
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	// password_hash is Optional but not Computed, so an unset one stays null: the hash of a
	// plaintext password is deliberately not surfaced, since it would appear in a plan as a
	// value the user never wrote.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, err := r.findUserByID(ctx, state.ID.ValueString())
	if errors.Is(err, errGone) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the user", err.Error())
		return
	}

	state.Email = types.StringValue(doc.Email)
	state.Username = types.StringValue(doc.Username)
	state.Name = types.StringValue(doc.Name)
	state.EmailVerified = types.BoolValue(doc.EmailVerified)
	state.AuthProvider = types.StringValue(doc.Provider)
	state.Role = types.StringValue(doc.Role)
	state.TermsAccepted = types.BoolValue(doc.TermsAccepted)
	state.CreatedAt = types.StringValue(doc.CreatedAt.UTC().Format(time.RFC3339))
	state.UpdatedAt = types.StringValue(doc.UpdatedAt.UTC().Format(time.RFC3339))

	// Password drift. bcrypt is salted, so the stored hash cannot be compared to a
	// previously stored hash - but it CAN be verified against the plaintext, which is the
	// one direction that works and is exactly what LibreChat does at login.
	//
	// Nulling the attribute is how a mismatch is reported. There is no plaintext to put in
	// state that would represent "whatever is in the database now", and null against a
	// configured value is a diff, which drives an Update that re-hashes. An account with no
	// password declared at all stays null on both sides and shows nothing.
	switch {
	case !state.Password.IsNull():
		if bcrypt.CompareHashAndPassword([]byte(doc.Password), []byte(state.Password.ValueString())) != nil {
			state.Password = types.StringNull()
		}
	case !state.PasswordHash.IsNull():
		// An explicit hash is written verbatim, so here a plain comparison is the right test.
		state.PasswordHash = optionalString(doc.Password)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	if ok := r.checkRole(ctx, plan.Role.ValueString(), &resp.Diagnostics); !ok {
		return
	}

	username := plan.Username.ValueString()
	if plan.Username.IsUnknown() || username == "" {
		username = defaultUsername(plan.Email.ValueString())
	}
	name := plan.Name.ValueString()
	if plan.Name.IsUnknown() || name == "" {
		name = username
	}

	now := time.Now().UTC()
	set := bson.M{
		"name":          name,
		"username":      username,
		"email":         plan.Email.ValueString(),
		"emailVerified": plan.EmailVerified.ValueBool(),
		"provider":      plan.AuthProvider.ValueString(),
		"role":          plan.Role.ValueString(),
		"termsAccepted": plan.TermsAccepted.ValueBool(),
		"updatedAt":     now,
	}

	update := bson.M{"$set": set}

	// Only re-hash when the password actually changed. Hashing on every apply would write a
	// new salt each time, which is invisible in the plan but invalidates every issued
	// refresh token - so an unrelated apply would log everybody out.
	if !plan.Password.Equal(state.Password) || !plan.PasswordHash.Equal(state.PasswordHash) {
		hash, herr := passwordHashFor(plan)
		if herr != nil {
			resp.Diagnostics.AddError("Cannot hash the password", herr.Error())
			return
		}
		if hash == "" {
			// Both attributes were removed. Unset the field rather than storing "", which
			// bcrypt reads as a malformed hash on every login attempt.
			update["$unset"] = bson.M{"password": ""}
		} else {
			set["password"] = hash
		}
	}

	if _, err := r.client.users().UpdateByID(ctx, id, update); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			resp.Diagnostics.AddError("That email is taken",
				fmt.Sprintf("Another account already uses %q.", plan.Email.ValueString()))
			return
		}
		resp.Diagnostics.AddError("Cannot update the user", err.Error())
		return
	}

	plan.ID = state.ID
	plan.Username = types.StringValue(username)
	plan.Name = types.StringValue(name)
	plan.CreatedAt = state.CreatedAt
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	// Grants made TO this account go with it. Left behind they are rows pointing at a
	// principal that no longer resolves - harmless to LibreChat, but they accumulate and
	// they make the aclentries collection impossible to audit.
	if _, err := r.client.aclEntries().DeleteMany(ctx, bson.M{
		"principalType": principalTypeUser,
		"principalId":   id,
	}); err != nil {
		resp.Diagnostics.AddError("Cannot remove the user's ACL grants", err.Error())
		return
	}

	if _, err := r.client.users().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot delete the user", err.Error())
		return
	}
}

// ImportState takes either the ObjectId or the email address, because the id of an account
// somebody registered through the web interface is not something they can easily find,
// whereas the address they typed is.
func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ident := strings.TrimSpace(req.ID)

	if _, err := bson.ObjectIDFromHex(ident); err == nil {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), ident)...)
		return
	}

	var doc userDoc
	err := r.client.users().FindOne(ctx, bson.M{"email": strings.ToLower(ident)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError(
			"No such user",
			fmt.Sprintf("Database %q has no account with email %q, and %q is not an ObjectId either.",
				r.client.DatabaseName(), ident, ident),
		)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the user", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), doc.ID.Hex())...)
}

func (r *userResource) findUserByID(ctx context.Context, hexID string) (*userDoc, error) {
	id, err := parseObjectID(hexID)
	if err != nil {
		return nil, err
	}

	var doc userDoc
	// The schema marks password `select: false`, which only affects mongoose - a raw driver
	// query returns it, and the Read path needs it to detect drift.
	err = r.client.users().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, errGone
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// checkRole refuses a role name the roles collection does not have. Mongo would store the
// string without complaint, and the account would come up with no permissions at all -
// which looks like a LibreChat bug rather than a typo in a tfvars file.
func (r *userResource) checkRole(ctx context.Context, name string, diags *diagnosticsSink) bool {
	exists, err := r.client.roleExists(ctx, name)
	if err != nil {
		diags.AddError("Cannot check the role", err.Error())
		return false
	}
	if !exists {
		known, _ := r.client.knownRoleNames(ctx)
		diags.AddError(
			"No such role",
			fmt.Sprintf("The roles collection has no %q. Known roles: %s.\n\nLibreChat seeds "+
				"USER and ADMIN itself; anything else has to be created with librechat_role first.",
				name, strings.Join(known, ", ")),
		)
		return false
	}
	return true
}

// passwordHashFor returns the hash to store, or "" when the account has no local password.
func passwordHashFor(m userResourceModel) (string, error) {
	if !m.PasswordHash.IsNull() && !m.PasswordHash.IsUnknown() {
		return m.PasswordHash.ValueString(), nil
	}
	if m.Password.IsNull() || m.Password.IsUnknown() {
		return "", nil
	}
	// Go's bcrypt emits the $2a$ prefix. bcryptjs, which LibreChat uses, accepts $2a$, $2b$
	// and $2y$ and reads the cost out of the hash, so this verifies against the application
	// exactly as a hash it wrote itself would.
	hash, err := bcrypt.GenerateFromPassword([]byte(m.Password.ValueString()), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// defaultUsername mirrors what a person would pick: the part of the address before the @.
func defaultUsername(email string) string {
	if at := strings.IndexByte(email, '@'); at > 0 {
		return email[:at]
	}
	return email
}
