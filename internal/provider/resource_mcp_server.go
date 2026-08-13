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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Transports LibreChat can speak to an MCP server. sse is legacy but still supported.
var validMCPTypes = []string{"streamable-http", "sse", "stdio", "websocket"}

// Keys of the `config` sub-document that LibreChat writes itself after connecting to the
// server. They are its cache of the introspection result, not configuration, and an update
// must leave them alone: overwriting them forces a reconnect and, until it succeeds, the
// agent's tool list is empty.
var mcpCachedConfigKeys = []string{"capabilities", "tools", "toolFunctions", "initDuration"}

var (
	_ resource.Resource                = (*mcpServerResource)(nil)
	_ resource.ResourceWithConfigure   = (*mcpServerResource)(nil)
	_ resource.ResourceWithImportState = (*mcpServerResource)(nil)
)

type mcpServerResource struct {
	client *Client
}

func NewMCPServerResource() resource.Resource { return &mcpServerResource{} }

type mcpServerResourceModel struct {
	ID            types.String `tfsdk:"id"`
	ServerName    types.String `tfsdk:"server_name"`
	Title         types.String `tfsdk:"title"`
	Type          types.String `tfsdk:"type"`
	URL           types.String `tfsdk:"url"`
	Timeout       types.Int64  `tfsdk:"timeout"`
	InitTimeout   types.Int64  `tfsdk:"init_timeout"`
	Headers       types.Map    `tfsdk:"headers"`
	RequiresOAuth types.Bool   `tfsdk:"requires_oauth"`
	AuthorID      types.String `tfsdk:"author_id"`
	CreatedAt     types.String `tfsdk:"created_at"`
	UpdatedAt     types.String `tfsdk:"updated_at"`
}

type mcpServerDoc struct {
	ID         bson.ObjectID `bson:"_id"`
	ServerName string        `bson:"serverName"`
	Config     bson.M        `bson:"config"`
	Author     bson.ObjectID `bson:"author"`
	CreatedAt  time.Time     `bson:"createdAt"`
	UpdatedAt  time.Time     `bson:"updatedAt"`
}

func (r *mcpServerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server"
}

func (r *mcpServerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An MCP server as a document in the `mcpservers` collection.\n\n" +
			"Declaring a server here rather than in `librechat.yaml` is the whole point: a server " +
			"in the config file is global and ownerless, so every user gets it. A document is an " +
			"ACL-managed resource, which is the only form group sharing can express - pair it with " +
			"`librechat_grant`.\n\n" +
			"Only the authored fields are written. LibreChat fills in the rest of `config` when it " +
			"first connects (`capabilities`, `tools`, `toolFunctions`, `initDuration`) and this " +
			"resource preserves whatever it cached, so an apply does not throw away a working " +
			"introspection.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "ObjectId of the document - what `librechat_grant.resource_id` " +
					"expects with `resource_type = \"mcpServer\"`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"server_name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The server's name, and it matters twice: it is the identity the " +
					"ACL attaches to, and it is the suffix LibreChat appends to every tool it " +
					"discovers. A server named `dummy` produces tools called `ping_mcp_dummy`, " +
					"`add_mcp_dummy` and so on - which is what an agent's `tools` list " +
					"has to contain. Renaming it therefore breaks every agent referencing those tools, " +
					"so it replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"title": schema.StringAttribute{
				Optional: true,
				// Computed as well as Optional because an omitted title is stored as the server
				// name rather than as nothing - LibreChat shows this field, and an empty label is a
				// blank row in the picker. Without Computed, state would keep the null the
				// configuration wrote while the document held "dummy", and every plan would carry a
				// diff that applying could not resolve.
				Computed:            true,
				MarkdownDescription: "Human-readable label shown in the interface. Defaults to `server_name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"type": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("streamable-http"),
				MarkdownDescription: "Transport. One of `" + strings.Join(validMCPTypes, "`, `") + "`.",
				Validators:          []validator.String{stringvalidator.OneOf(validMCPTypes...)},
			},
			"url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute URL of the MCP endpoint. Resolved by LibreChat inside " +
					"its container, so a hostname has to be one that container can resolve - a " +
					"Docker service name, not `localhost`.",
				Validators: []validator.String{stringvalidator.RegexMatches(
					urlPattern, "must be an absolute http(s) URL")},
			},
			"timeout": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Per-request timeout in milliseconds. Unset leaves LibreChat's default.",
			},
			"init_timeout": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Timeout in milliseconds for the initial handshake. Worth raising " +
					"for a server that loads data at start-up.",
			},
			"headers": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "Headers sent with every request, which is how a bearer token " +
					"reaches an authenticated MCP server:\n\n" +
					"```hcl\nheaders = { Authorization = \"Bearer ${var.mcp_token}\" }\n```\n\n" +
					"Read the token from a secret store rather than writing it here - the value lands " +
					"in the document, readable by anyone with database access, and in Terraform state.\n\n" +
					"Declared headers **replace** what is stored rather than merging, so removing one " +
					"here removes it from the server too.",
			},
			"requires_oauth": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Whether LibreChat should run its OAuth flow before connecting. " +
					"Leave false when authenticating with a static header.",
			},
			"author_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ObjectId of the account recorded as author. Required by the " +
					"schema; it grants nothing - permissions come from `librechat_grant`.",
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{Computed: true},
		},
	}
}

func (r *mcpServerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *mcpServerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan mcpServerResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	author, ok := r.checkAuthor(ctx, plan.AuthorID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}

	config, ok := r.buildConfig(ctx, plan, nil, &resp.Diagnostics)
	if !ok {
		return
	}

	now := time.Now().UTC()
	res, err := r.client.mcpServers().InsertOne(ctx, bson.M{
		"serverName": plan.ServerName.ValueString(),
		"config":     config,
		"author":     author,
		"createdAt":  now,
		"updatedAt":  now,
		"__v":        0,
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			resp.Diagnostics.AddError("That MCP server already exists",
				fmt.Sprintf("A server named %q is already in the database. Adopt it rather than "+
					"recreating it:\n\n  tofu import <this resource address> %s\n\nOriginal error: %s",
					plan.ServerName.ValueString(), plan.ServerName.ValueString(), err))
			return
		}
		resp.Diagnostics.AddError("Cannot create the MCP server", err.Error())
		return
	}

	id, ok2 := res.InsertedID.(bson.ObjectID)
	if !ok2 {
		resp.Diagnostics.AddError("Unexpected inserted id",
			fmt.Sprintf("MongoDB returned %T instead of an ObjectId. This is a bug in the provider.", res.InsertedID))
		return
	}

	plan.ID = types.StringValue(id.Hex())
	plan.Title = types.StringValue(resolvedTitle(plan))
	plan.CreatedAt = types.StringValue(now.Format(time.RFC3339))
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *mcpServerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state mcpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	var doc mcpServerDoc
	err = r.client.mcpServers().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the MCP server", err.Error())
		return
	}

	state.ServerName = types.StringValue(doc.ServerName)
	state.AuthorID = types.StringValue(doc.Author.Hex())
	state.CreatedAt = types.StringValue(doc.CreatedAt.UTC().Format(time.RFC3339))
	state.UpdatedAt = types.StringValue(doc.UpdatedAt.UTC().Format(time.RFC3339))

	// Not optionalString: title is Computed and always stored, so "" would be a document
	// written by something other than this provider rather than an unset attribute.
	state.Title = types.StringValue(configString(doc.Config, "title"))
	state.URL = types.StringValue(configString(doc.Config, "url"))
	if t := configString(doc.Config, "type"); t != "" {
		state.Type = types.StringValue(t)
	}
	state.RequiresOAuth = types.BoolValue(configBool(doc.Config, "requiresOAuth"))
	state.Timeout = configInt64(doc.Config, "timeout")
	state.InitTimeout = configInt64(doc.Config, "initTimeout")

	headers := configStringMap(doc.Config, "headers")
	if state.Headers.IsNull() && len(headers) == 0 {
		state.Headers = types.MapNull(types.StringType)
	} else {
		m, d := types.MapValueFrom(ctx, types.StringType, headers)
		resp.Diagnostics.Append(d...)
		state.Headers = m
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *mcpServerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var plan, state mcpServerResourceModel
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

	author, ok := r.checkAuthor(ctx, plan.AuthorID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}

	// Read the stored config first, so the introspection cache survives the write. See
	// mcpCachedConfigKeys.
	var existing mcpServerDoc
	err = r.client.mcpServers().FindOne(ctx, bson.M{"_id": id}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("The MCP server is gone",
			"It was deleted between the plan and the apply. Re-run to recreate it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the MCP server", err.Error())
		return
	}

	config, ok := r.buildConfig(ctx, plan, existing.Config, &resp.Diagnostics)
	if !ok {
		return
	}

	now := time.Now().UTC()
	if _, err := r.client.mcpServers().UpdateByID(ctx, id, bson.M{"$set": bson.M{
		"config":    config,
		"author":    author,
		"updatedAt": now,
	}}); err != nil {
		resp.Diagnostics.AddError("Cannot update the MCP server", err.Error())
		return
	}

	plan.ID = state.ID
	plan.Title = types.StringValue(resolvedTitle(plan))
	plan.CreatedAt = state.CreatedAt
	plan.UpdatedAt = types.StringValue(now.Format(time.RFC3339))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *mcpServerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !clientReady(r.client, &resp.Diagnostics) {
		return
	}

	var state mcpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseObjectID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Corrupt state", err.Error())
		return
	}

	if err := r.client.deleteGrantsForResource(ctx, "mcpServer", id); err != nil {
		resp.Diagnostics.AddError("Cannot remove the server's ACL grants", err.Error())
		return
	}

	if _, err := r.client.mcpServers().DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		resp.Diagnostics.AddError("Cannot delete the MCP server", err.Error())
	}
}

func (r *mcpServerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ident := strings.TrimSpace(req.ID)

	if _, err := bson.ObjectIDFromHex(ident); err == nil {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), ident)...)
		return
	}

	var doc mcpServerDoc
	err := r.client.mcpServers().FindOne(ctx, bson.M{"serverName": ident}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such MCP server",
			fmt.Sprintf("Database %q has no server named %q, and %q is not an ObjectId either.",
				r.client.DatabaseName(), ident, ident))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the MCP server", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, pathRoot("id"), doc.ID.Hex())...)
}

// buildConfig produces the config sub-document, starting from whatever LibreChat has cached.
func (r *mcpServerResource) buildConfig(ctx context.Context, plan mcpServerResourceModel, existing bson.M, diags *diagnosticsSink) (bson.M, bool) {
	config := bson.M{}
	// Carry over only the cache. Every other key in a stored config is one this resource
	// authors, so copying the whole document would resurrect a field the configuration just
	// removed.
	for _, key := range mcpCachedConfigKeys {
		if v, ok := existing[key]; ok {
			config[key] = v
		}
	}

	config["title"] = resolvedTitle(plan)
	config["type"] = plan.Type.ValueString()
	config["url"] = plan.URL.ValueString()
	config["requiresOAuth"] = plan.RequiresOAuth.ValueBool()
	// "user" marks the server as one owned by a principal rather than declared globally in
	// librechat.yaml. It is what makes the ACL apply at all.
	config["source"] = "user"

	// Present-and-null are different things for these two. Writing an explicit null where a
	// number is expected is how an omitted timeout became a connection that never gave up.
	if !plan.Timeout.IsNull() && !plan.Timeout.IsUnknown() {
		config["timeout"] = plan.Timeout.ValueInt64()
	}
	if !plan.InitTimeout.IsNull() && !plan.InitTimeout.IsUnknown() {
		config["initTimeout"] = plan.InitTimeout.ValueInt64()
	}

	headers := mapToStringMap(ctx, plan.Headers, diags)
	if diags.HasError() {
		return nil, false
	}
	if headers != nil {
		config["headers"] = headers
	}

	return config, true
}

// resolvedTitle is what actually gets stored, which state has to agree with - see the note on
// the title attribute.
func resolvedTitle(plan mcpServerResourceModel) string {
	if plan.Title.IsNull() || plan.Title.IsUnknown() || plan.Title.ValueString() == "" {
		return plan.ServerName.ValueString()
	}
	return plan.Title.ValueString()
}

func (r *mcpServerResource) checkAuthor(ctx context.Context, hex string, diags *diagnosticsSink) (bson.ObjectID, bool) {
	id, err := parseObjectID(hex)
	if err != nil {
		diags.AddAttributeError(pathRoot("author_id"), "Not a user id", err.Error())
		return bson.NilObjectID, false
	}
	if err := r.client.users().FindOne(ctx, bson.M{"_id": id}).Err(); errors.Is(err, mongo.ErrNoDocuments) {
		diags.AddAttributeError(pathRoot("author_id"), "No such user", fmt.Sprintf(
			"There is no account with id %s in database %q.", hex, r.client.DatabaseName()))
		return bson.NilObjectID, false
	} else if err != nil {
		diags.AddError("Cannot verify the author", err.Error())
		return bson.NilObjectID, false
	}
	return id, true
}

// The config sub-document is Mixed, so every read out of it has to cope with a missing key
// and with whatever type MongoDB chose to store a number as.

func configString(config bson.M, key string) string {
	if v, ok := config[key].(string); ok {
		return v
	}
	return ""
}

func configBool(config bson.M, key string) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return false
}

// configInt64 accepts every numeric type BSON might hand back: a value written as an int64
// comes back as int64, but one written by LibreChat's JavaScript comes back as a double.
func configInt64(config bson.M, key string) types.Int64 {
	switch v := config[key].(type) {
	case int64:
		return types.Int64Value(v)
	case int32:
		return types.Int64Value(int64(v))
	case float64:
		return types.Int64Value(int64(v))
	default:
		return types.Int64Null()
	}
}

func configStringMap(config bson.M, key string) map[string]string {
	raw, ok := config[key].(bson.M)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
