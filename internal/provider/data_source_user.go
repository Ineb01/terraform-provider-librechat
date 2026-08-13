package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	_ datasource.DataSource              = (*userDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*userDataSource)(nil)
)

type userDataSource struct {
	client *Client
}

func NewUserDataSource() datasource.DataSource { return &userDataSource{} }

type userDataSourceModel struct {
	ID            types.String `tfsdk:"id"`
	Email         types.String `tfsdk:"email"`
	Username      types.String `tfsdk:"username"`
	Name          types.String `tfsdk:"name"`
	Role          types.String `tfsdk:"role"`
	EmailVerified types.Bool   `tfsdk:"email_verified"`
}

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing LibreChat account.\n\n" +
			"The usual reason to need this is the `author_id` on an agent or MCP server: those " +
			"require a real account ObjectId, and on a database where the first admin registered " +
			"through the web interface - LibreChat hashes passwords, so that is often the only way " +
			"the first account can come to exist - Terraform never created it and has no reference " +
			"to it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The account's ObjectId.",
			},
			"email": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Look up by email. Exactly one of `email` or `username` is required.",
			},
			"username": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Look up by username. Exactly one of `email` or `username` is required.",
			},
			"name": schema.StringAttribute{Computed: true},
			"role": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The role the account holds - `ADMIN` here is admin access.",
			},
			"email_verified": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "False means the account cannot log in yet.",
			},
		},
	}
}

func (d *userDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if !clientReady(d.client, &resp.Diagnostics) {
		return
	}

	var config userDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	hasEmail := !config.Email.IsNull() && !config.Email.IsUnknown()
	hasUsername := !config.Username.IsNull() && !config.Username.IsUnknown()

	var filter bson.M
	var lookedUpBy string
	switch {
	case hasEmail && hasUsername:
		resp.Diagnostics.AddError("Set only one of email and username",
			"Both were given. Two filters would silently disagree - an address and a handle "+
				"belonging to different accounts - so only one is accepted.")
		return
	case hasEmail:
		filter = bson.M{"email": config.Email.ValueString()}
		lookedUpBy = fmt.Sprintf("email %q", config.Email.ValueString())
	case hasUsername:
		filter = bson.M{"username": config.Username.ValueString()}
		lookedUpBy = fmt.Sprintf("username %q", config.Username.ValueString())
	default:
		resp.Diagnostics.AddError("Set either email or username",
			"Neither was given, so there is nothing to look up.")
		return
	}

	var doc userDoc
	err := d.client.users().FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such user", fmt.Sprintf(
			"Database %q has no account with %s.\n\nLibreChat hashes passwords, so an account "+
				"that has to be able to log in cannot be written from outside the application - if "+
				"this is the first admin, register it in the web interface and apply again. "+
				"Alternatively let librechat_user create it, which does hash the password properly.",
			d.client.DatabaseName(), lookedUpBy))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the user", err.Error())
		return
	}

	config.ID = types.StringValue(doc.ID.Hex())
	config.Email = types.StringValue(doc.Email)
	config.Username = types.StringValue(doc.Username)
	config.Name = types.StringValue(doc.Name)
	config.Role = types.StringValue(doc.Role)
	config.EmailVerified = types.BoolValue(doc.EmailVerified)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
