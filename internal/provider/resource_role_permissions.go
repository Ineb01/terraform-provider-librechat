package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// privateKeyPrevious holds the values these permission fields had before Terraform first
// touched them, so Delete can put them back. Private state rather than an attribute: it is
// bookkeeping, it would be noise in every plan, and nobody should be configuring it.
const privateKeyPrevious = "previous_permissions"

var (
	_ resource.Resource                = (*rolePermissionsResource)(nil)
	_ resource.ResourceWithConfigure   = (*rolePermissionsResource)(nil)
	_ resource.ResourceWithImportState = (*rolePermissionsResource)(nil)
)

type rolePermissionsResource struct {
	client *Client
}

func NewRolePermissionsResource() resource.Resource { return &rolePermissionsResource{} }

type rolePermissionsResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Role        types.String `tfsdk:"role"`
	Permissions types.Map    `tfsdk:"permissions"`
}

func (r *rolePermissionsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role_permissions"
}

func (r *rolePermissionsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Forces named permission fields on a role LibreChat owns - `USER` or " +
			"`ADMIN` - without taking over the rest of the document.\n\n" +
			"This is the counterpart to `librechat_role`: that resource owns a whole custom role, " +
			"this one patches individual fields of a role the application seeds and re-reconciles " +
			"on every start-up. Only the fields named here are written, so a permission a newer " +
			"LibreChat adds keeps its own default.\n\n" +
			"`librechat.yaml` cannot express this. Its `interface` block writes only the `USE` bit " +
			"of each permission type, so `agents: false` there would take away *using* agents as " +
			"well as creating them.\n\n" +
			"It applies to the ROLE, so it reaches every account holding that role - there is no " +
			"per-user form. Destroying the resource restores whatever each field held before " +
			"Terraform first set it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The role name; there is one of these per role.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"role": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of an existing role, typically `USER` or `ADMIN`. It must " +
					"already exist - this resource patches a role, it does not create one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"permissions": schema.MapAttribute{
				Required:    true,
				ElementType: types.MapType{ElemType: types.BoolType},
				MarkdownDescription: "Permission type to permission to boolean. Only what is named " +
					"here is written:\n\n" +
					"```hcl\npermissions = {\n  AGENTS      = { CREATE = false, SHARE = false }\n  MCP_SERVERS = { CREATE = false }\n}\n```\n\n" +
					"That example is the common one: it stops ordinary accounts building their own " +
					"agents while leaving them able to use every agent granted to them, which is what " +
					"keeps a Terraform-managed estate from filling up with hand-made copies.\n\n" +
					"An unknown permission **type** is rejected rather than created: MongoDB would " +
					"store `AGENT` for `AGENTS` without complaint, and the configuration would then " +
					"read as if it restricted something while the real permission stayed as it was. " +
					"Individual permission names are deliberately not checked the same way, because a " +
					"role document written by an older LibreChat can legitimately be missing a field " +
					"this one has.",
			},
		},
	}
}

func (r *rolePermissionsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *rolePermissionsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan rolePermissionsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	declared := permissionsFromMap(ctx, plan.Permissions, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, ok := r.fetchRole(ctx, plan.Role.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	if !r.checkPermissionTypes(doc, declared, &resp.Diagnostics) {
		return
	}

	previous := snapshot(doc.Permissions, declared, nil)
	encoded, err := json.Marshal(previous)
	if err != nil {
		resp.Diagnostics.AddError("Cannot record the previous permissions", err.Error())
		return
	}

	if !r.write(ctx, doc.ID, declared, &resp.Diagnostics) {
		return
	}

	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateKeyPrevious, encoded)...)
	plan.ID = types.StringValue(doc.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *rolePermissionsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state rolePermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	declared := permissionsFromMap(ctx, state.Permissions, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	var doc roleDoc
	err := r.client.roles().FindOne(ctx, bson.M{"name": state.Role.ValueString()}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the role", err.Error())
		return
	}

	// Refresh only the fields the configuration named - the rest of the role document is
	// LibreChat's and is none of this resource's business. A field that has since vanished
	// from the document is left out of the refreshed map, which reads as a diff and drives an
	// update that writes it again.
	current := map[string]map[string]bool{}
	for permType, perms := range declared {
		for permName := range perms {
			stored, ok := doc.Permissions[permType][permName]
			if !ok {
				continue
			}
			if current[permType] == nil {
				current[permType] = map[string]bool{}
			}
			current[permType][permName] = stored
		}
	}

	state.ID = types.StringValue(doc.Name)
	state.Permissions = permissionsToMap(ctx, current, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *rolePermissionsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan rolePermissionsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	declared := permissionsFromMap(ctx, plan.Permissions, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, ok := r.fetchRole(ctx, plan.Role.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	if !r.checkPermissionTypes(doc, declared, &resp.Diagnostics) {
		return
	}

	// Extend the snapshot rather than replacing it. A field newly added to the configuration
	// needs its pre-Terraform value recorded now; a field that was already managed must keep
	// the value captured the first time, not the one Terraform itself wrote.
	previous := map[string]map[string]*bool{}
	if raw, diags := req.Private.GetKey(ctx, privateKeyPrevious); len(raw) > 0 {
		resp.Diagnostics.Append(diags...)
		if err := json.Unmarshal(raw, &previous); err != nil {
			resp.Diagnostics.AddWarning(
				"Could not read the recorded previous permissions",
				"Destroying this resource will restore only the fields it can still account for. "+
					"Original error: "+err.Error())
			previous = map[string]map[string]*bool{}
		}
	}
	previous = snapshot(doc.Permissions, declared, previous)
	encoded, err := json.Marshal(previous)
	if err != nil {
		resp.Diagnostics.AddError("Cannot record the previous permissions", err.Error())
		return
	}

	if !r.write(ctx, doc.ID, declared, &resp.Diagnostics) {
		return
	}

	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateKeyPrevious, encoded)...)
	plan.ID = types.StringValue(doc.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete restores what each managed field held before Terraform set it. A permission has no
// "absent" state that means anything on its own, so the alternative would be to leave the
// last Terraform-written value behind - which makes a destroy fail to undo what the apply did.
func (r *rolePermissionsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state rolePermissionsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	raw, diags := req.Private.GetKey(ctx, privateKeyPrevious)
	resp.Diagnostics.Append(diags...)
	if len(raw) == 0 {
		// Nothing recorded: an imported resource, or state written by a version of this
		// provider without the snapshot. Leaving the values in place is the only safe option,
		// and saying so beats silently doing nothing.
		resp.Diagnostics.AddWarning(
			"Permissions left as they are",
			fmt.Sprintf("No record of what role %q held before Terraform set these permissions, so "+
				"they are being left at their current values rather than guessed at. Check them in "+
				"the admin interface if that is not what you want.", state.Role.ValueString()))
		return
	}

	var previous map[string]map[string]*bool
	if err := json.Unmarshal(raw, &previous); err != nil {
		resp.Diagnostics.AddError("Cannot read the recorded previous permissions", err.Error())
		return
	}

	var doc roleDoc
	err := r.client.roles().FindOne(ctx, bson.M{"name": state.Role.ValueString()}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// The role itself is gone, so there is nothing to restore.
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the role", err.Error())
		return
	}

	set := bson.M{}
	unset := bson.M{}
	for permType, perms := range previous {
		for permName, value := range perms {
			field := "permissions." + permType + "." + permName
			if value == nil {
				// The field did not exist before. Removing it rather than writing false lets
				// LibreChat's own default apply again on the next start-up.
				unset[field] = ""
			} else {
				set[field] = *value
			}
		}
	}

	update := bson.M{}
	if len(set) > 0 {
		update["$set"] = set
	}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if len(update) == 0 {
		return
	}

	if _, err := r.client.roles().UpdateByID(ctx, doc.ID, update); err != nil {
		resp.Diagnostics.AddError("Cannot restore the previous permissions", err.Error())
	}
}

// ImportState takes the role name. Note the warning in Delete: an imported resource has no
// record of the pre-Terraform values, so destroying it will not restore them.
func (r *rolePermissionsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name := strings.TrimSpace(req.ID)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("role"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), name)...)
}

func (r *rolePermissionsResource) fetchRole(ctx context.Context, name string, diags *diagnosticsSink) (*roleDoc, bool) {
	var doc roleDoc
	err := r.client.roles().FindOne(ctx, bson.M{"name": name}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		known, _ := r.client.knownRoleNames(ctx)
		diags.AddAttributeError(pathRoot("role"), "No such role",
			fmt.Sprintf("The roles collection has no %q. Known roles: %s.\n\nThis resource patches "+
				"an existing role; use librechat_role to create one.", name, strings.Join(known, ", ")))
		return nil, false
	}
	if err != nil {
		diags.AddError("Cannot read the role", err.Error())
		return nil, false
	}
	return &doc, true
}

// checkPermissionTypes rejects a permission type the role document does not have. See the
// note on the permissions attribute for why the check stops at the type.
func (r *rolePermissionsResource) checkPermissionTypes(doc *roleDoc, declared map[string]map[string]bool, diags *diagnosticsSink) bool {
	known := make([]string, 0, len(doc.Permissions))
	for permType := range doc.Permissions {
		known = append(known, permType)
	}
	sort.Strings(known)

	for permType := range declared {
		if _, ok := doc.Permissions[permType]; !ok {
			diags.AddAttributeError(pathRoot("permissions"), "Unknown permission type",
				fmt.Sprintf("Role %q has no permission type %q. Types on this role: %s.",
					doc.Name, permType, strings.Join(known, ", ")))
			return false
		}
	}
	return true
}

// write sets the named fields one dotted path at a time, so nothing else in the permissions
// sub-document is disturbed.
func (r *rolePermissionsResource) write(ctx context.Context, roleID bson.ObjectID, declared map[string]map[string]bool, diags *diagnosticsSink) bool {
	set := bson.M{}
	for permType, perms := range declared {
		for permName, value := range perms {
			set["permissions."+permType+"."+permName] = value
		}
	}
	if len(set) == 0 {
		return true
	}

	if _, err := r.client.roles().UpdateByID(ctx, roleID, bson.M{"$set": set}); err != nil {
		diags.AddError("Cannot write the role permissions", err.Error())
		return false
	}
	return true
}

// snapshot records the current value of every declared field, keeping anything already
// recorded in `into`. A nil value means the field was not present at all.
func snapshot(stored map[string]map[string]bool, declared map[string]map[string]bool, into map[string]map[string]*bool) map[string]map[string]*bool {
	if into == nil {
		into = map[string]map[string]*bool{}
	}
	for permType, perms := range declared {
		for permName := range perms {
			if _, already := into[permType][permName]; already {
				continue
			}
			if into[permType] == nil {
				into[permType] = map[string]*bool{}
			}
			if value, ok := stored[permType][permName]; ok {
				v := value
				into[permType][permName] = &v
			} else {
				into[permType][permName] = nil
			}
		}
	}
	return into
}
