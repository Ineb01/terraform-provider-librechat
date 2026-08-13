package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	_ resource.Resource                = (*agentResource)(nil)
	_ resource.ResourceWithConfigure   = (*agentResource)(nil)
	_ resource.ResourceWithImportState = (*agentResource)(nil)
)

type agentResource struct {
	client *Client
}

func NewAgentResource() resource.Resource { return &agentResource{} }

type agentResourceModel struct {
	ID                    types.String         `tfsdk:"id"`
	AgentID               types.String         `tfsdk:"agent_id"`
	Name                  types.String         `tfsdk:"name"`
	Description           types.String         `tfsdk:"description"`
	Instructions          types.String         `tfsdk:"instructions"`
	ModelProvider         types.String         `tfsdk:"model_provider"`
	Model                 types.String         `tfsdk:"model"`
	ModelParameters       jsontypes.Normalized `tfsdk:"model_parameters"`
	Tools                 types.List           `tfsdk:"tools"`
	MCPServerNames        types.List           `tfsdk:"mcp_server_names"`
	Category              types.String         `tfsdk:"category"`
	ConversationStarters  types.List           `tfsdk:"conversation_starters"`
	AuthorID              types.String         `tfsdk:"author_id"`
	AuthorName            types.String         `tfsdk:"author_name"`
	Artifacts             types.String         `tfsdk:"artifacts"`
	IsPromoted            types.Bool           `tfsdk:"is_promoted"`
	EndAfterTools         types.Bool           `tfsdk:"end_after_tools"`
	HideSequentialOutputs types.Bool           `tfsdk:"hide_sequential_outputs"`
	RecursionLimit        types.Int64          `tfsdk:"recursion_limit"`
	SupportContactName    types.String         `tfsdk:"support_contact_name"`
	SupportContactEmail   types.String         `tfsdk:"support_contact_email"`
	ToolOptions           jsontypes.Normalized `tfsdk:"tool_options"`
	CreatedAt             types.String         `tfsdk:"created_at"`
	UpdatedAt             types.String         `tfsdk:"updated_at"`
}

type agentDoc struct {
	MongoID              bson.ObjectID   `bson:"_id"`
	ID                   string          `bson:"id"`
	Name                 string          `bson:"name"`
	Description          string          `bson:"description"`
	Instructions         string          `bson:"instructions"`
	Provider             string          `bson:"provider"`
	Model                string          `bson:"model"`
	ModelParameters      bson.M          `bson:"model_parameters"`
	Tools                []string        `bson:"tools"`
	MCPServerNames       []string        `bson:"mcpServerNames"`
	Category             string          `bson:"category"`
	ConversationStarters []string        `bson:"conversation_starters"`
	Author               bson.ObjectID   `bson:"author"`
	AuthorName           string          `bson:"authorName"`
	Artifacts            string          `bson:"artifacts"`
	IsPromoted           bool            `bson:"is_promoted"`
	EndAfterTools        bool            `bson:"end_after_tools"`
	HideSeqOutputs       bool            `bson:"hide_sequential_outputs"`
	RecursionLimit       *int64          `bson:"recursion_limit"`
	SupportContact       *supportContact `bson:"support_contact"`
	ToolOptions          bson.M          `bson:"tool_options"`
	CreatedAt            time.Time       `bson:"createdAt"`
	UpdatedAt            time.Time       `bson:"updatedAt"`
}

type supportContact struct {
	Name  string `bson:"name"`
	Email string `bson:"email"`
}

func (r *agentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_agent"
}

func (r *agentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A LibreChat agent, written to the `agents` collection.\n\n" +
			"Creating the agent does **not** make it visible to anybody. Access is ACL-managed: " +
			"pair this with `librechat_grant` resources, and grant ownership to the `ADMIN` " +
			"**role** rather than to the author's account, so a second admin created later " +
			"inherits it and deleting one admin orphans nothing. The `author_id` field names a " +
			"user only because the schema requires an ObjectId there; it confers no permissions.\n\n" +
			"Destroying an agent removes its ACL rows too - LibreChat has no cascade, and rows " +
			"pointing at a deleted agent are impossible to audit.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "ObjectId of the document. This - not `agent_id` - is what " +
					"`librechat_grant.resource_id` expects.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"agent_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The identity LibreChat uses everywhere in its API and in " +
					"conversation records, conventionally `agent_<something>`. Changing it replaces " +
					"the agent, which orphans every conversation that referenced the old id, so pick " +
					"it once and leave it alone.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Display name in the agent picker.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "One-line description shown under the name.",
			},
			"instructions": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The system prompt. Keep it in a file and read it with " +
					"`file(\"${path.module}/files/agent-x.md\")` rather than inline, so the prompt is " +
					"diffable and reviewable on its own.",
			},
			"model_provider": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "LibreChat's `provider` field: which configured endpoint serves " +
					"the model, e.g. `bedrock` or `bedrock`. Named `model_provider` here only because " +
					"`provider` is a reserved meta-argument in a resource block.",
			},
			"model": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Model name as the chosen endpoint knows it.",
			},
			"model_parameters": schema.StringAttribute{
				CustomType: jsontypes.NormalizedType{},
				Optional:   true,
				MarkdownDescription: "Sampling and limit parameters as a JSON object, e.g. " +
					"`jsonencode({ temperature = 0.2, max_tokens = 4096 })`. Free-form because " +
					"LibreChat types it as `Object` and the accepted keys differ per endpoint.\n\n" +
					"Note that JSON numbers round-trip through BSON as doubles, so writing `1000.0` " +
					"where the refresh reads back `1000` shows a one-time diff. Write plain integers.",
			},
			"tools": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Tool names the agent may call. MCP tools are suffixed with the " +
					"server name, so a `librechat_mcp_server` named `dummy` contributes tools called " +
					"`ping_mcp_dummy`, `add_mcp_dummy` and so on - the suffix is part of the " +
					"name and must appear here.",
			},
			"mcp_server_names": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "MCP servers this agent uses, by `server_name`. LibreChat " +
					"maintains this alongside `tools` as a query index; it is written explicitly here " +
					"rather than derived, because the derivation lives in application code this " +
					"provider does not run.",
			},
			"category": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("general"),
				MarkdownDescription: "Grouping in the agent marketplace.",
			},
			"conversation_starters": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Example prompts offered on an empty conversation.",
			},
			"author_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ObjectId of the account recorded as author (`librechat_user.x.id`, " +
					"or `data.librechat_user.x.id` for an account somebody registered by hand). " +
					"Required by the schema; it grants nothing. Permissions come from `librechat_grant`.",
			},
			"author_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Name shown as the author in the marketplace. Cosmetic.",
			},
			"artifacts": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "LibreChat's artifacts mode for this agent; empty disables it.",
			},
			"is_promoted": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Feature the agent at the top of the marketplace.",
			},
			"end_after_tools": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Stop the run after tool calls instead of letting the model summarise them.",
			},
			"hide_sequential_outputs": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Hide intermediate outputs of chained agents.",
			},
			"recursion_limit": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Maximum agent-loop iterations. Unset uses LibreChat's own default.",
			},
			"support_contact_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Name shown to users who need help with this agent.",
			},
			"support_contact_email": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Address shown to users who need help with this agent.",
			},
			"tool_options": schema.StringAttribute{
				CustomType: jsontypes.NormalizedType{},
				Optional:   true,
				MarkdownDescription: "Per-tool configuration (`defer_loading`, `allowed_callers`) as a " +
					"JSON object keyed by tool name. Free-form because LibreChat types it as Mixed.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *agentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *agentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan agentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, ok := r.buildDoc(ctx, plan, time.Now().UTC(), time.Time{}, &resp.Diagnostics)
	if !ok {
		return
	}

	res, err := r.client.agents().InsertOne(ctx, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			resp.Diagnostics.AddError(
				"That agent already exists",
				fmt.Sprintf("An agent with id %q is already in the database. Adopt it rather than "+
					"recreating it:\n\n  tofu import <this resource address> %s\n\nOriginal error: %s",
					plan.AgentID.ValueString(), plan.AgentID.ValueString(), err),
			)
			return
		}
		resp.Diagnostics.AddError("Cannot create the agent", err.Error())
		return
	}

	id, ok := res.InsertedID.(bson.ObjectID)
	if !ok {
		resp.Diagnostics.AddError("Unexpected inserted id",
			fmt.Sprintf("MongoDB returned %T instead of an ObjectId. This is a bug in the provider.", res.InsertedID))
		return
	}

	plan.ID = types.StringValue(id.Hex())
	stamp := doc["createdAt"].(time.Time).Format(time.RFC3339)
	plan.CreatedAt = types.StringValue(stamp)
	plan.UpdatedAt = types.StringValue(stamp)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *agentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	var doc agentDoc
	err = r.client.agents().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the agent", err.Error())
		return
	}

	state.AgentID = types.StringValue(doc.ID)
	state.Name = types.StringValue(doc.Name)
	state.Description = optionalString(doc.Description)
	state.Instructions = optionalString(doc.Instructions)
	state.ModelProvider = types.StringValue(doc.Provider)
	state.Model = types.StringValue(doc.Model)
	state.Category = types.StringValue(doc.Category)
	state.AuthorID = types.StringValue(doc.Author.Hex())
	state.AuthorName = optionalString(doc.AuthorName)
	state.Artifacts = optionalString(doc.Artifacts)
	state.IsPromoted = types.BoolValue(doc.IsPromoted)
	state.EndAfterTools = types.BoolValue(doc.EndAfterTools)
	state.HideSequentialOutputs = types.BoolValue(doc.HideSeqOutputs)
	state.CreatedAt = types.StringValue(doc.CreatedAt.UTC().Format(time.RFC3339))
	state.UpdatedAt = types.StringValue(doc.UpdatedAt.UTC().Format(time.RFC3339))

	if doc.RecursionLimit != nil {
		state.RecursionLimit = types.Int64Value(*doc.RecursionLimit)
	} else {
		state.RecursionLimit = types.Int64Null()
	}

	if doc.SupportContact != nil {
		state.SupportContactName = optionalString(doc.SupportContact.Name)
		state.SupportContactEmail = optionalString(doc.SupportContact.Email)
	} else {
		state.SupportContactName = types.StringNull()
		state.SupportContactEmail = types.StringNull()
	}

	// The list attributes are Optional and not Computed, so a never-declared list has to stay
	// null: LibreChat's schema defaults these to [], and refreshing null into [] would put a
	// permanent diff in every plan.
	state.Tools = refreshOptionalList(ctx, state.Tools, doc.Tools, &resp.Diagnostics)
	state.MCPServerNames = refreshOptionalList(ctx, state.MCPServerNames, doc.MCPServerNames, &resp.Diagnostics)
	state.ConversationStarters = refreshOptionalList(ctx, state.ConversationStarters, doc.ConversationStarters, &resp.Diagnostics)

	state.ModelParameters = refreshOptionalJSON(state.ModelParameters, doc.ModelParameters, &resp.Diagnostics)
	state.ToolOptions = refreshOptionalJSON(state.ToolOptions, doc.ToolOptions, &resp.Diagnostics)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *agentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state agentResourceModel
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

	created, err := time.Parse(time.RFC3339, state.CreatedAt.ValueString())
	if err != nil {
		// Not fatal: createdAt is cosmetic, and refusing the update over an unparseable
		// timestamp would be worse than leaving the field as it is.
		created = time.Time{}
	}

	now := time.Now().UTC()
	doc, ok := r.buildDoc(ctx, plan, now, created, &resp.Diagnostics)
	if !ok {
		return
	}
	// createdAt is preserved by buildDoc; _id must not be in a $set.
	delete(doc, "__v")

	if _, err := r.client.agents().UpdateByID(ctx, id, bson.M{"$set": doc}); err != nil {
		resp.Diagnostics.AddError("Cannot update the agent", err.Error())
		return
	}

	plan.ID = state.ID
	plan.CreatedAt = state.CreatedAt
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *agentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state agentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	if err := r.client.deleteGrantsForResource(ctx, "agent", id); err != nil {
		resp.Diagnostics.AddError("Cannot remove the agent's ACL grants", err.Error())
		return
	}

	if _, err := r.client.agents().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot delete the agent", err.Error())
	}
}

// ImportState accepts the LibreChat agent id ("agent_helpdesk") or the document's ObjectId.
// The agent id is the one visible in the interface's URL, so it is what someone actually has.
func (r *agentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ident := strings.TrimSpace(req.ID)

	if _, err := bson.ObjectIDFromHex(ident); err == nil {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), ident)...)
		return
	}

	var doc agentDoc
	err := r.client.agents().FindOne(ctx, bson.M{"id": ident}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such agent",
			fmt.Sprintf("Database %q has no agent with id %q, and %q is not an ObjectId either.",
				r.client.DatabaseName(), ident, ident))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the agent", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), doc.MongoID.Hex())...)
}

// buildDoc renders the authored fields. createdAt is carried through so the same builder
// serves Create and Update; an update must not reset it.
func (r *agentResource) buildDoc(ctx context.Context, plan agentResourceModel, now, created time.Time, diags *diagnosticsSink) (bson.M, bool) {
	author, err := parseObjectID(plan.AuthorID.ValueString())
	if err != nil {
		diags.AddAttributeError(pathRoot("author_id"), "Not a user id", err.Error())
		return nil, false
	}
	if err := r.client.users().FindOne(ctx, bson.M{"_id": author}).Err(); errors.Is(err, mongo.ErrNoDocuments) {
		diags.AddAttributeError(pathRoot("author_id"), "No such user", fmt.Sprintf(
			"There is no account with id %s in database %q. The schema requires a real ObjectId "+
				"here, so an agent cannot be created before its author exists.",
			plan.AuthorID.ValueString(), r.client.DatabaseName()))
		return nil, false
	} else if err != nil {
		diags.AddError("Cannot verify the author", err.Error())
		return nil, false
	}

	modelParams, ok := decodeJSONObject(plan.ModelParameters, "model_parameters", diags)
	if !ok {
		return nil, false
	}
	toolOptions, ok := decodeJSONObject(plan.ToolOptions, "tool_options", diags)
	if !ok {
		return nil, false
	}

	tools := listToSlice(ctx, plan.Tools, diags)
	mcpNames := listToSlice(ctx, plan.MCPServerNames, diags)
	starters := listToSlice(ctx, plan.ConversationStarters, diags)
	if diags.HasError() {
		return nil, false
	}

	if created.IsZero() {
		created = now
	}

	support := bson.M{
		"name":  plan.SupportContactName.ValueString(),
		"email": plan.SupportContactEmail.ValueString(),
	}

	doc := bson.M{
		"id":                      plan.AgentID.ValueString(),
		"name":                    plan.Name.ValueString(),
		"description":             plan.Description.ValueString(),
		"instructions":            plan.Instructions.ValueString(),
		"provider":                plan.ModelProvider.ValueString(),
		"model":                   plan.Model.ValueString(),
		"model_parameters":        modelParams,
		"artifacts":               plan.Artifacts.ValueString(),
		"tools":                   tools,
		"mcpServerNames":          mcpNames,
		"conversation_starters":   starters,
		"category":                plan.Category.ValueString(),
		"author":                  author,
		"is_promoted":             plan.IsPromoted.ValueBool(),
		"end_after_tools":         plan.EndAfterTools.ValueBool(),
		"hide_sequential_outputs": plan.HideSequentialOutputs.ValueBool(),
		"support_contact":         support,
		// Written explicitly because mongoose's defaults do not apply to a document inserted
		// from outside the application, and LibreChat reads several of these without a null
		// guard.
		"tool_kwargs":    []any{},
		"tool_options":   toolOptions,
		"agent_ids":      []string{},
		"edges":          []any{},
		"tool_resources": bson.M{},
		"createdAt":      created,
		"updatedAt":      now,
		"__v":            0,
	}

	if !plan.AuthorName.IsNull() && !plan.AuthorName.IsUnknown() {
		doc["authorName"] = plan.AuthorName.ValueString()
	}
	if !plan.RecursionLimit.IsNull() && !plan.RecursionLimit.IsUnknown() {
		doc["recursion_limit"] = plan.RecursionLimit.ValueInt64()
	}

	// The application appends a snapshot to `versions` on every edit, and the interface's
	// revert feature reads it. Terraform is the only writer here, so a history would just
	// restate this module's git log: one entry, rewritten each apply, keeps the field valid
	// without pretending to be an audit trail.
	version := bson.M{
		"id":                    plan.AgentID.ValueString(),
		"name":                  plan.Name.ValueString(),
		"description":           plan.Description.ValueString(),
		"instructions":          plan.Instructions.ValueString(),
		"model_parameters":      modelParams,
		"provider":              plan.ModelProvider.ValueString(),
		"model":                 plan.Model.ValueString(),
		"tools":                 tools,
		"category":              plan.Category.ValueString(),
		"artifacts":             plan.Artifacts.ValueString(),
		"edges":                 []any{},
		"support_contact":       support,
		"conversation_starters": starters,
		"createdAt":             created,
		"updatedAt":             now,
	}
	doc["versions"] = []bson.M{version}

	return doc, true
}

// decodeJSONObject turns a JSON attribute into a BSON document. An absent attribute becomes
// an empty document rather than null, because LibreChat indexes into both of these fields
// without a null guard.
func decodeJSONObject(v jsontypes.Normalized, attribute string, diags *diagnosticsSink) (bson.M, bool) {
	if v.IsNull() || v.IsUnknown() {
		return bson.M{}, true
	}

	var out bson.M
	if err := json.Unmarshal([]byte(v.ValueString()), &out); err != nil {
		diags.AddAttributeError(pathRoot(attribute), "Not a JSON object",
			fmt.Sprintf("%s must be a JSON object, not an array or a scalar: %s", attribute, err))
		return nil, false
	}
	return out, true
}

// refreshOptionalList keeps an Optional list null when it was null and the database agrees
// it is empty. See the comment at the call site.
func refreshOptionalList(ctx context.Context, current types.List, fromDB []string, diags *diagnosticsSink) types.List {
	if current.IsNull() && len(fromDB) == 0 {
		return types.ListNull(types.StringType)
	}
	return stringList(ctx, fromDB, diags)
}

// refreshOptionalJSON is the same idea for the free-form JSON attributes.
func refreshOptionalJSON(current jsontypes.Normalized, fromDB bson.M, diags *diagnosticsSink) jsontypes.Normalized {
	if current.IsNull() && len(fromDB) == 0 {
		return jsontypes.NewNormalizedNull()
	}
	encoded, err := json.Marshal(fromDB)
	if err != nil {
		diags.AddError("Cannot encode a stored JSON field", err.Error())
		return current
	}
	return jsontypes.NewNormalizedValue(string(encoded))
}
