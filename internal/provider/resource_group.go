package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	_ resource.Resource                = (*groupResource)(nil)
	_ resource.ResourceWithConfigure   = (*groupResource)(nil)
	_ resource.ResourceWithImportState = (*groupResource)(nil)
)

type groupResource struct {
	client *Client
}

func NewGroupResource() resource.Resource { return &groupResource{} }

type groupResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Email       types.String `tfsdk:"email"`
	MemberIDs   types.Set    `tfsdk:"member_ids"`
	CreatedAt   types.String `tfsdk:"created_at"`
	UpdatedAt   types.String `tfsdk:"updated_at"`
}

type groupDoc struct {
	ID          bson.ObjectID `bson:"_id"`
	Name        string        `bson:"name"`
	Description string        `bson:"description"`
	Email       string        `bson:"email"`
	MemberIDs   []string      `bson:"memberIds"`
	Source      string        `bson:"source"`
	CreatedAt   time.Time     `bson:"createdAt"`
	UpdatedAt   time.Time     `bson:"updatedAt"`
}

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A local LibreChat group. Groups are what make sharing worth doing: " +
			"an agent or MCP server granted to a group is usable by its members without anyone " +
			"being named individually, and membership changes without touching the grant.\n\n" +
			"Only `source = \"local\"` groups are managed here. A group synced from Entra ID is " +
			"owned by that sync and must not be adopted.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "ObjectId of the group. This is what a `librechat_grant` with `principal_type = \"group\"` expects.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Group name, shown in the sharing dialog.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free text describing who the group is for.",
			},
			"email": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Optional address associated with the group; LibreChat indexes but does not send to it.",
			},
			"member_ids": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Member accounts, as ObjectId strings - `librechat_user.x.id`.\n\n" +
					"These are stored as **strings**, not as ObjectIds, because that is what " +
					"LibreChat's schema declares (`memberIds: [String]`) and how it resolves " +
					"membership: `Group.find({ memberIds: user.idOnTheSource || String(user._id) })`. " +
					"An ObjectId written here matches nothing - the user resolves to zero groups, " +
					"every grant made to the group quietly stops applying, and nothing errors. The " +
					"provider always writes the string form, so referencing `librechat_user.x.id` is " +
					"correct.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan groupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	members := setToSlice(ctx, plan.MemberIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.checkMembers(ctx, members, &resp.Diagnostics) {
		return
	}

	now := time.Now().UTC()
	res, err := r.client.groups().InsertOne(ctx, bson.M{
		"name":        plan.Name.ValueString(),
		"description": plan.Description.ValueString(),
		"email":       plan.Email.ValueString(),
		"memberIds":   members,
		// Hardcoded: an "entra" group belongs to the directory sync, and the schema then
		// requires idOnTheSource. Managing one from Terraform would fight the sync.
		"source":    "local",
		"createdAt": now,
		"updatedAt": now,
		"__v":       0,
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot create the group", err.Error())
		return
	}

	id, ok := res.InsertedID.(bson.ObjectID)
	if !ok {
		resp.Diagnostics.AddError("Unexpected inserted id",
			fmt.Sprintf("MongoDB returned %T instead of an ObjectId. This is a bug in the provider.", res.InsertedID))
		return
	}

	plan.ID = types.StringValue(id.Hex())
	plan.MemberIDs = stringSet(ctx, members, &resp.Diagnostics)
	plan.CreatedAt = types.StringValue(now.Format(time.RFC3339))
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state groupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	var doc groupDoc
	err = r.client.groups().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the group", err.Error())
		return
	}

	state.Name = types.StringValue(doc.Name)
	state.Description = optionalString(doc.Description)
	state.Email = optionalString(doc.Email)
	// member_ids is Optional and not Computed, so a group that never declared members must
	// stay null rather than becoming an empty set - otherwise every plan shows a diff between
	// null in the configuration and [] in state.
	if state.MemberIDs.IsNull() && len(doc.MemberIDs) == 0 {
		state.MemberIDs = types.SetNull(types.StringType)
	} else {
		state.MemberIDs = stringSet(ctx, doc.MemberIDs, &resp.Diagnostics)
	}
	state.CreatedAt = types.StringValue(doc.CreatedAt.UTC().Format(time.RFC3339))
	state.UpdatedAt = types.StringValue(doc.UpdatedAt.UTC().Format(time.RFC3339))

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state groupResourceModel
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

	members := setToSlice(ctx, plan.MemberIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !r.checkMembers(ctx, members, &resp.Diagnostics) {
		return
	}

	now := time.Now().UTC()
	// Only the authored fields. avatar and idOnTheSource are LibreChat's, and a replaceOne
	// would drop them.
	if _, err := r.client.groups().UpdateByID(ctx, id, bson.M{"$set": bson.M{
		"name":        plan.Name.ValueString(),
		"description": plan.Description.ValueString(),
		"email":       plan.Email.ValueString(),
		"memberIds":   members,
		"updatedAt":   now,
	}}); err != nil {
		resp.Diagnostics.AddError("Cannot update the group", err.Error())
		return
	}

	plan.ID = state.ID
	plan.CreatedAt = state.CreatedAt
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state groupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	// Grants made to this group would otherwise stay, pointing at a group id nothing
	// resolves. Harmless to LibreChat, but they make the collection unauditable, and a
	// recreated group with the same name gets a new id so they would never apply again.
	if _, err := r.client.aclEntries().DeleteMany(ctx, bson.M{
		"principalType": principalTypeGroup,
		"principalId":   id,
	}); err != nil {
		resp.Diagnostics.AddError("Cannot remove the group's ACL grants", err.Error())
		return
	}

	if _, err := r.client.groups().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot delete the group", err.Error())
	}
}

// ImportState accepts the ObjectId or the group name; group names are unique in practice
// and far easier to find than the id.
func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ident := strings.TrimSpace(req.ID)

	if _, err := bson.ObjectIDFromHex(ident); err == nil {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), ident)...)
		return
	}

	var doc groupDoc
	err := r.client.groups().FindOne(ctx, bson.M{"name": ident, "source": "local"}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such group",
			fmt.Sprintf("Database %q has no local group named %q, and %q is not an ObjectId either.",
				r.client.DatabaseName(), ident, ident))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the group", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), doc.ID.Hex())...)
}

// checkMembers verifies every member id resolves to a real account, for the reason spelled
// out on the member_ids attribute: an unresolvable member is not an error anywhere in
// LibreChat, it is just a person who never sees the group's agents.
func (r *groupResource) checkMembers(ctx context.Context, members []string, diags *diagnosticsSink) bool {
	for _, hex := range members {
		id, err := parseObjectID(hex)
		if err != nil {
			diags.AddAttributeError(pathRoot("member_ids"), "Not a user id", err.Error()+
				"\n\nmember_ids holds account ObjectIds - reference librechat_user.x.id, not an email or username.")
			return false
		}
		if err := r.client.users().FindOne(ctx, bson.M{"_id": id}).Err(); errors.Is(err, mongo.ErrNoDocuments) {
			diags.AddAttributeError(pathRoot("member_ids"), "No such user",
				fmt.Sprintf("There is no account with id %s in database %q.", hex, r.client.DatabaseName()))
			return false
		} else if err != nil {
			diags.AddError("Cannot verify group members", err.Error())
			return false
		}
	}
	return true
}
