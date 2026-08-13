package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// pathRoot is a shorthand; the provider refers to top-level attributes constantly and
// path.Root reads poorly inline.
func pathRoot(name string) path.Path { return path.Root(name) }

// clientFrom unwraps the *Client that Configure stashed in ProviderData. providerData is
// nil when the framework calls Configure on a resource before the provider itself has been
// configured, which is normal and must not be reported as an error.
func clientFrom(providerData any, diags *diag.Diagnostics) *Client {
	if providerData == nil {
		return nil
	}
	client, ok := providerData.(*Client)
	if !ok {
		diags.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *provider.Client, got %T. This is a bug in the provider.", providerData),
		)
		return nil
	}
	return client
}

// clientReady guards every CRUD entry point against a nil client.
//
// It is reachable, not defensive boilerplate. Configure returns without a client when
// mongo_uri is still unknown - which is what lets a configuration create MongoDB and populate
// it in a single apply - and OpenTofu can then still ask for a refresh in the same run, for
// instance when the resource the URI derives from is being replaced. Without this the provider
// panics, and a panicking plugin gives the user a stack trace instead of a reason.
func clientReady(client *Client, diags *diag.Diagnostics) bool {
	if client != nil {
		return true
	}
	diags.AddError(
		"The librechat provider is not configured yet",
		"Its mongo_uri is derived from a resource that does not exist yet, so there is no "+
			"connection to work with.\n\n"+
			"This is normal during the first apply of a configuration that creates MongoDB and "+
			"fills it in one go, and OpenTofu resolves it by itself. Seeing it as a hard error "+
			"usually means a refresh was attempted while that resource was being replaced - "+
			"applying again is generally enough. To avoid the dependency entirely, set "+
			"LIBRECHAT_MONGO_URI to a fixed address instead.",
	)
	return false
}

// listToSlice converts a Terraform list of strings to a Go slice. A null or unknown list
// becomes an empty slice rather than nil, because these end up in fields whose LibreChat
// schema default is []: writing null where the app expects an array is how an agent with
// no tools stopped loading entirely.
func listToSlice(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	out := []string{}
	if l.IsNull() || l.IsUnknown() {
		return out
	}
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// setToSlice is the concrete form for types.Set.
func setToSlice(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	out := []string{}
	if s.IsNull() || s.IsUnknown() {
		return out
	}
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	return out
}

// mapToStringMap converts a Terraform map of strings, returning nil for an absent map.
// nil matters here, unlike for the slices above: an absent headers map means "remove the
// headers", which is a different write from "set them to {}".
func mapToStringMap(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}

// stringList builds a types.List for state. Always a known, non-null list for the same
// reason stringSlice never returns nil.
func stringList(ctx context.Context, in []string, diags *diag.Diagnostics) types.List {
	if in == nil {
		in = []string{}
	}
	l, d := types.ListValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	return l
}

// stringSet builds a types.Set for state.
func stringSet(ctx context.Context, in []string, diags *diag.Diagnostics) types.Set {
	if in == nil {
		in = []string{}
	}
	s, d := types.SetValueFrom(ctx, types.StringType, in)
	diags.Append(d...)
	return s
}

// optionalString maps "" back to null so that an attribute the configuration never set
// does not show up in state as an empty string and produce a permanent diff.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
