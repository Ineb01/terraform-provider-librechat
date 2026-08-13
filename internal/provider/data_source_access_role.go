package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*accessRoleDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*accessRoleDataSource)(nil)
)

type accessRoleDataSource struct {
	client *Client
}

func NewAccessRoleDataSource() datasource.DataSource { return &accessRoleDataSource{} }

type accessRoleDataSourceModel struct {
	ID           types.String `tfsdk:"id"`
	ResourceType types.String `tfsdk:"resource_type"`
	Name         types.String `tfsdk:"name"`
	AccessRoleID types.String `tfsdk:"access_role_id"`
	DisplayName  types.String `tfsdk:"display_name"`
	Description  types.String `tfsdk:"description"`
	PermBits     types.Int64  `tfsdk:"perm_bits"`
}

func (d *accessRoleDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_access_role"
}

func (d *accessRoleDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one of LibreChat's ACL permission templates out of the " +
			"`accessroles` collection - `agent_viewer`, `mcpServer_owner` and so on.\n\n" +
			"`librechat_grant` resolves these itself, so this data source is not needed to make a " +
			"grant. It is here for the times the question is \"what does `editor` actually allow\": " +
			"`perm_bits` is the bitmask LibreChat's permission checks compare against, and reading " +
			"it is faster and more reliable than reasoning about which bits a release assigns.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "ObjectId of the accessroles document.",
			},
			"resource_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "One of `" + strings.Join(validResourceTypes, "`, `") + "`.",
				Validators:          []validator.String{stringvalidator.OneOf(validResourceTypes...)},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Short role name - `viewer`, `editor`, `owner`. Combined with " +
					"`resource_type` into the stored `accessRoleId`.",
			},
			"access_role_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The composed identifier actually stored, e.g. `agent_viewer`.",
			},
			"display_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The name LibreChat shows in the sharing dialog.",
			},
			"description": schema.StringAttribute{Computed: true},
			"perm_bits": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "The permission bitmask. At the time of writing VIEW = 1, " +
					"EDIT = 2, DELETE = 4, SHARE = 8, which makes `viewer` 1, `editor` 3 and " +
					"`owner` 15 - but read the value rather than assuming it, which is the point of " +
					"this data source.",
			},
		},
	}
}

func (d *accessRoleDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *accessRoleDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if !clientReady(d.client, &resp.Diagnostics) {
		return
	}

	var config accessRoleDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, err := d.client.lookupAccessRole(ctx,
		config.ResourceType.ValueString(), config.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the access role", err.Error())
		return
	}

	config.ID = types.StringValue(doc.ID.Hex())
	config.AccessRoleID = types.StringValue(doc.AccessRoleID)
	config.DisplayName = types.StringValue(doc.Name)
	config.Description = optionalString(doc.Description)
	config.PermBits = types.Int64Value(doc.PermBits)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
