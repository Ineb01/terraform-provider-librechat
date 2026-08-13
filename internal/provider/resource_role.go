package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	_ resource.Resource                = (*roleResource)(nil)
	_ resource.ResourceWithConfigure   = (*roleResource)(nil)
	_ resource.ResourceWithImportState = (*roleResource)(nil)
)

type roleResource struct {
	client *Client
}

func NewRoleResource() resource.Resource { return &roleResource{} }

type roleResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Permissions types.Map    `tfsdk:"permissions"`
}

type roleDoc struct {
	ID          bson.ObjectID              `bson:"_id"`
	Name        string                     `bson:"name"`
	Description string                     `bson:"description"`
	Permissions map[string]map[string]bool `bson:"permissions"`
}

func (r *roleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role"
}

func (r *roleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A **custom** role in the `roles` collection, owned outright by " +
			"Terraform.\n\n" +
			"Use this only for a role LibreChat does not ship. `USER` and `ADMIN` are seeded and " +
			"then re-reconciled by LibreChat on every start-up, so managing either one as a whole " +
			"document means fighting the application: use `librechat_role_permissions` for those, " +
			"which patches named fields and leaves the rest alone.\n\n" +
			"A role only takes effect once an account holds it - set `librechat_user.role`. " +
			"Destroying a role that accounts still hold is refused, because an account whose role " +
			"does not resolve loses every permission and the interface gives no hint why.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Role name. This is the value that goes in a user's `role` " +
					"attribute and in a `librechat_grant` with `principal_type = \"role\"`. Renaming " +
					"replaces the role, which would leave any account still holding the old name " +
					"without permissions, so it is a replace and not an update on purpose.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free text.",
			},
			"permissions": schema.MapAttribute{
				Optional:    true,
				ElementType: types.MapType{ElemType: types.BoolType},
				MarkdownDescription: "What accounts holding this role may DO, as permission type to " +
					"permission to boolean:\n\n" +
					"```hcl\npermissions = {\n  AGENTS      = { USE = true, CREATE = false }\n  MCP_SERVERS = { USE = true, CREATE = false }\n}\n```\n\n" +
					"This is a different question from a `librechat_grant`: a grant says who may use " +
					"one particular agent, this says whether an account may build one at all. " +
					"LibreChat enforces it in middleware - `POST /api/agents` wants `AGENTS.USE` and " +
					"`AGENTS.CREATE` - so clearing `CREATE` leaves an account able to use every agent " +
					"shared with it and unable to make its own.\n\n" +
					"Permission types and names come from LibreChat's own schema (`AGENTS`, `PROMPTS`, " +
					"`MEMORIES`, `MCP_SERVERS`, `PEOPLE_PICKER`, ...). An unknown type is rejected " +
					"rather than created, because MongoDB would store the typo happily and the " +
					"configuration would read as if it restricted something it never touched.",
			},
		},
	}
}

func (r *roleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *roleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan roleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	permissions := permissionsFromMap(ctx, plan.Permissions, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()

	// LibreChat seeds its own roles on start-up, and this resource replaces a document
	// wholesale - so adopting one it owns would silently drop whichever permission fields the
	// configuration did not mention, and LibreChat would put them back on the next restart.
	// Naming the alternative is the useful part of the error.
	exists, err := r.client.roleExists(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Cannot check for an existing role", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddError(
			"That role already exists",
			fmt.Sprintf("The roles collection already has %q.\n\n"+
				"If it is one LibreChat seeds (USER, ADMIN), do not manage it with librechat_role - "+
				"use librechat_role_permissions, which writes only the fields you name.\n\n"+
				"If it is a custom role created earlier, adopt it:\n\n"+
				"  tofu import <this resource address> %s", name, name),
		)
		return
	}

	doc := bson.M{"name": name, "description": plan.Description.ValueString(), "__v": 0}
	if permissions != nil {
		doc["permissions"] = permissions
	}

	res, err := r.client.roles().InsertOne(ctx, doc)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create the role", err.Error())
		return
	}

	id, ok := res.InsertedID.(bson.ObjectID)
	if !ok {
		resp.Diagnostics.AddError("Unexpected inserted id",
			fmt.Sprintf("MongoDB returned %T instead of an ObjectId. This is a bug in the provider.", res.InsertedID))
		return
	}

	plan.ID = types.StringValue(id.Hex())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	var doc roleDoc
	err = r.client.roles().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the role", err.Error())
		return
	}

	state.Name = types.StringValue(doc.Name)
	state.Description = optionalString(doc.Description)

	if state.Permissions.IsNull() && len(doc.Permissions) == 0 {
		state.Permissions = types.MapNull(types.MapType{ElemType: types.BoolType})
	} else {
		state.Permissions = permissionsToMap(ctx, doc.Permissions, &resp.Diagnostics)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state roleResourceModel
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

	permissions := permissionsFromMap(ctx, plan.Permissions, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if permissions == nil {
		permissions = map[string]map[string]bool{}
	}

	// A whole-document write of the permissions sub-document, not a merge: this resource owns
	// the role, so a permission type dropped from the configuration has to disappear rather
	// than linger.
	if _, err := r.client.roles().UpdateByID(ctx, id, bson.M{"$set": bson.M{
		"description": plan.Description.ValueString(),
		"permissions": permissions,
	}}); err != nil {
		resp.Diagnostics.AddError("Cannot update the role", err.Error())
		return
	}

	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}
	name := state.Name.ValueString()

	// Refuse while accounts still hold the role. Mongo would delete it without complaint and
	// those accounts would come up with no permissions at all - which reads as a LibreChat
	// fault, not as a consequence of this destroy.
	holders, err := r.roleHolders(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Cannot check who holds the role", err.Error())
		return
	}
	if len(holders) > 0 {
		resp.Diagnostics.AddError(
			"The role is still in use",
			fmt.Sprintf("These accounts still have role %q: %s.\n\nMove them to another role "+
				"first. Deleting the role underneath them leaves each one with no permissions and "+
				"no indication why.", name, strings.Join(holders, ", ")),
		)
		return
	}

	// Grants made to the role name go with it, for the same reason as for users and groups.
	if _, err := r.client.aclEntries().DeleteMany(ctx, bson.M{
		"principalType": principalTypeRole,
		"principalId":   name,
	}); err != nil {
		resp.Diagnostics.AddError("Cannot remove the role's ACL grants", err.Error())
		return
	}

	if _, err := r.client.roles().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot delete the role", err.Error())
	}
}

func (r *roleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ident := strings.TrimSpace(req.ID)

	if _, err := bson.ObjectIDFromHex(ident); err == nil {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), ident)...)
		return
	}

	var doc roleDoc
	err := r.client.roles().FindOne(ctx, bson.M{"name": ident}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such role",
			fmt.Sprintf("Database %q has no role named %q, and %q is not an ObjectId either.",
				r.client.DatabaseName(), ident, ident))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the role", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), doc.ID.Hex())...)
}

// roleHolders lists the emails of accounts with this role, capped: the message only needs to
// make the problem concrete, and a role held by five hundred people would produce an error
// nobody can read.
func (r *roleResource) roleHolders(ctx context.Context, name string) ([]string, error) {
	cur, err := r.client.users().Find(ctx, bson.M{"role": name})
	if err != nil {
		return nil, err
	}
	var docs []struct {
		Email string `bson:"email"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}

	const maxListed = 10
	out := make([]string, 0, len(docs))
	for i, d := range docs {
		if i == maxListed {
			out = append(out, fmt.Sprintf("and %d more", len(docs)-maxListed))
			break
		}
		out = append(out, d.Email)
	}
	return out, nil
}

// permissionsFromMap converts the nested Terraform map. Returns nil for an absent map, which
// the callers distinguish from an empty one.
func permissionsFromMap(ctx context.Context, m types.Map, diags *diagnosticsSink) map[string]map[string]bool {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := map[string]map[string]bool{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}

func permissionsToMap(ctx context.Context, in map[string]map[string]bool, diags *diagnosticsSink) types.Map {
	if in == nil {
		in = map[string]map[string]bool{}
	}
	m, d := types.MapValueFrom(ctx, types.MapType{ElemType: types.BoolType}, in)
	diags.Append(d...)
	return m
}
