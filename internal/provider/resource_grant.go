package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	_ resource.Resource                = (*grantResource)(nil)
	_ resource.ResourceWithConfigure   = (*grantResource)(nil)
	_ resource.ResourceWithImportState = (*grantResource)(nil)
)

type grantResource struct {
	client *Client
}

func NewGrantResource() resource.Resource { return &grantResource{} }

type grantResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ResourceType  types.String `tfsdk:"resource_type"`
	ResourceID    types.String `tfsdk:"resource_id"`
	AccessRole    types.String `tfsdk:"access_role"`
	PrincipalType types.String `tfsdk:"principal_type"`
	PrincipalID   types.String `tfsdk:"principal_id"`
	GrantedBy     types.String `tfsdk:"granted_by"`
	PermBits      types.Int64  `tfsdk:"perm_bits"`
}

func (r *grantResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_grant"
}

func (r *grantResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One row of LibreChat's `aclentries` collection: who may do what " +
			"with one agent, MCP server or other shareable resource. This is the answer to " +
			"\"who can use this agent\", which is a different question from " +
			"`librechat_role_permissions` (\"may this account build an agent of its own\").\n\n" +
			"Ownership is best granted to the **ADMIN role** rather than to a person: every " +
			"admin, including one created later, then has full rights without anything being " +
			"re-granted, and deleting one admin account orphans nothing.\n\n" +
			"Grant no more than `viewer` to anyone who should not edit a Terraform-managed " +
			"resource - `editor` carries the EDIT bit, which is what LibreChat checks before " +
			"allowing a PATCH from the interface, and an edit there is drift this provider will " +
			"overwrite on the next apply.\n\n" +
			"Terraform only manages the grants it created. A grant added by hand in the " +
			"interface is left alone rather than revoked, so it will not appear in a plan; if " +
			"exclusive control matters, audit `aclentries` for the resource id.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "ObjectId of the ACL row.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"resource_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "What kind of thing is being shared. One of: `" + strings.Join(validResourceTypes, "`, `") + "`.",
				Validators:          []validator.String{stringvalidator.OneOf(validResourceTypes...)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"resource_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ObjectId of the resource - `librechat_agent.x.id` or " +
					"`librechat_mcp_server.x.id`, **not** their `agent_id`/`server_name`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"access_role": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Permission template to apply, looked up in `accessroles` as " +
					"`<resource_type>_<access_role>`. For agents and MCP servers LibreChat seeds " +
					"`viewer`, `editor` and `owner`. The permission bits come from that document " +
					"rather than being hardcoded here, so a LibreChat release that adds a bit does " +
					"not leave this provider writing a stale bitmask.",
			},
			"principal_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Who is being granted access: `user`, `group`, `role`, or " +
					"`public` for everyone.",
				Validators: []validator.String{stringvalidator.OneOf(
					principalTypeUser, principalTypeGroup, principalTypeRole, principalTypePublic)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"principal_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Identifies the grantee, and the form depends on " +
					"`principal_type`:\n\n" +
					"- `user` - the account's ObjectId (`librechat_user.x.id`)\n" +
					"- `group` - the group's ObjectId (`librechat_group.x.id`)\n" +
					"- `role` - the role's **name**, e.g. `ADMIN`; not its ObjectId\n" +
					"- `public` - omitted entirely",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"granted_by": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ObjectId of the account recorded as having made the grant. " +
					"Audit metadata only - it confers no permissions of its own.",
			},
			"perm_bits": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "The bitmask actually stored, copied from the `accessroles` " +
					"document. Exposed because it is what LibreChat's permission checks compare " +
					"against, and it is the quickest way to confirm a grant means what was intended.",
			},
		},
	}
}

func (r *grantResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *grantResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan grantResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resolved, ok := r.resolve(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}

	// An upsert rather than an insert. The same grant may already exist - LibreChat writes
	// an owner row itself when an agent is created through the interface - and there is no
	// unique index to make a second one fail, so a plain insert would silently produce two
	// rows for one principal. Keyed on the identity the schema treats as unique in practice.
	filter := bson.M{
		"resourceType": resolved.resourceType,
		"resourceId":   resolved.resourceID,
	}
	for k, v := range resolved.principal.filter() {
		filter[k] = v
	}

	now := time.Now().UTC()
	set := bson.M{
		"resourceType": resolved.resourceType,
		"resourceId":   resolved.resourceID,
		"permBits":     resolved.accessRole.PermBits,
		"roleId":       resolved.accessRole.ID,
		"grantedAt":    now,
		"updatedAt":    now,
	}
	for k, v := range resolved.principal.fields() {
		set[k] = v
	}
	if resolved.grantedBy != nil {
		set["grantedBy"] = *resolved.grantedBy
	}

	res := r.client.aclEntries().FindOneAndUpdate(ctx, filter,
		bson.M{"$set": set, "$setOnInsert": bson.M{"createdAt": now, "__v": 0}},
		mongoUpsertReturningNew())

	var doc aclEntryDoc
	if err := res.Decode(&doc); err != nil {
		resp.Diagnostics.AddError("Cannot write the grant", err.Error())
		return
	}

	plan.ID = types.StringValue(doc.ID.Hex())
	plan.PermBits = types.Int64Value(resolved.accessRole.PermBits)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *grantResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	var doc aclEntryDoc
	err = r.client.aclEntries().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the grant", err.Error())
		return
	}

	state.ResourceType = types.StringValue(doc.ResourceType)
	state.ResourceID = types.StringValue(doc.ResourceID.Hex())
	state.PrincipalType = types.StringValue(doc.PrincipalType)
	state.PrincipalID = optionalString(doc.principalIDString())
	state.PermBits = types.Int64Value(doc.PermBits)
	if doc.GrantedBy != nil {
		state.GrantedBy = types.StringValue(doc.GrantedBy.Hex())
	} else {
		state.GrantedBy = types.StringNull()
	}

	// access_role is not stored on the row - roleId is. Resolving it back means a permission
	// template edited in the database shows up as a diff on the attribute that was actually
	// configured, instead of only as a changed perm_bits number.
	if doc.RoleID != nil {
		name, err := r.accessRoleName(ctx, *doc.RoleID, doc.ResourceType)
		if err == nil {
			state.AccessRole = types.StringValue(name)
		}
		// A roleId pointing at a deleted accessroles document leaves the configured value in
		// place: the grant is still there and still carries permBits, so removing the
		// resource from state would be wrong, and guessing a role name would be worse.
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *grantResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state grantResourceModel
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

	resolved, ok := r.resolve(ctx, plan, &resp.Diagnostics)
	if !ok {
		return
	}

	now := time.Now().UTC()
	set := bson.M{
		"permBits":  resolved.accessRole.PermBits,
		"roleId":    resolved.accessRole.ID,
		"updatedAt": now,
	}
	update := bson.M{"$set": set}
	if resolved.grantedBy != nil {
		set["grantedBy"] = *resolved.grantedBy
	} else {
		update["$unset"] = bson.M{"grantedBy": ""}
	}

	if _, err := r.client.aclEntries().UpdateByID(ctx, id, update); err != nil {
		resp.Diagnostics.AddError("Cannot update the grant", err.Error())
		return
	}

	plan.ID = state.ID
	plan.PermBits = types.Int64Value(resolved.accessRole.PermBits)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *grantResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state grantResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	if _, err := r.client.aclEntries().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot revoke the grant", err.Error())
	}
}

func (r *grantResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), strings.TrimSpace(req.ID))...)
}

// resolvedGrant is everything a write needs, after the configuration has been checked
// against the database.
type resolvedGrant struct {
	resourceType string
	resourceID   bson.ObjectID
	accessRole   *accessRoleDoc
	principal    principal
	grantedBy    *bson.ObjectID
}

func (r *grantResource) resolve(ctx context.Context, plan grantResourceModel, diags *diagnosticsSink) (resolvedGrant, bool) {
	var out resolvedGrant

	out.resourceType = plan.ResourceType.ValueString()

	resourceID, err := parseObjectID(plan.ResourceID.ValueString())
	if err != nil {
		diags.AddAttributeError(pathRoot("resource_id"), "Not a resource id", err.Error()+
			"\n\nUse the resource's `id` attribute. An agent has two identifiers: `agent_id` is "+
			"the name LibreChat uses in its API, `id` is the document's ObjectId, and an ACL row "+
			"references the latter.")
		return out, false
	}
	out.resourceID = resourceID

	out.accessRole, err = r.client.lookupAccessRole(ctx, out.resourceType, plan.AccessRole.ValueString())
	if err != nil {
		diags.AddAttributeError(pathRoot("access_role"), "Unknown access role", err.Error())
		return out, false
	}

	out.principal, err = r.client.resolvePrincipal(ctx,
		plan.PrincipalType.ValueString(), plan.PrincipalID.ValueString())
	if err != nil {
		diags.AddAttributeError(pathRoot("principal_id"), "Cannot resolve the principal", err.Error())
		return out, false
	}

	if !plan.GrantedBy.IsNull() && !plan.GrantedBy.IsUnknown() {
		id, err := parseObjectID(plan.GrantedBy.ValueString())
		if err != nil {
			diags.AddAttributeError(pathRoot("granted_by"), "Not a user id", err.Error())
			return out, false
		}
		out.grantedBy = &id
	}

	return out, true
}

// accessRoleName turns a stored roleId back into the short name used in configuration:
// the accessRoleId "agent_viewer" for resource type "agent" becomes "viewer".
func (r *grantResource) accessRoleName(ctx context.Context, roleID bson.ObjectID, resourceType string) (string, error) {
	var doc accessRoleDoc
	if err := r.client.accessRoles().FindOne(ctx, bson.M{"_id": roleID}).Decode(&doc); err != nil {
		return "", err
	}
	name := strings.TrimPrefix(doc.AccessRoleID, resourceType+"_")
	if name == "" {
		return "", fmt.Errorf("accessRoleId %q does not start with %q", doc.AccessRoleID, resourceType+"_")
	}
	return name, nil
}
