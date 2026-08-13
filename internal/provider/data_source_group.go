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
	_ datasource.DataSource              = (*groupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*groupDataSource)(nil)
)

type groupDataSource struct {
	client *Client
}

func NewGroupDataSource() datasource.DataSource { return &groupDataSource{} }

type groupDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Source      types.String `tfsdk:"source"`
	MemberIDs   types.Set    `tfsdk:"member_ids"`
}

func (d *groupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (d *groupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing LibreChat group by name, whatever created it.\n\n" +
			"This is how a grant is made to a group synced from Entra ID: that group belongs to the " +
			"directory sync and must not be managed with `librechat_group`, but its id is exactly " +
			"what `librechat_grant` needs.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The group's ObjectId - what a `librechat_grant` with `principal_type = \"group\"` expects.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Group name to look up.",
			},
			"description": schema.StringAttribute{Computed: true},
			"source": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`local` for a group created here or in the admin interface, " +
					"`entra` for one owned by the directory sync.",
			},
			"member_ids": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Members, as the strings LibreChat stores - account ObjectIds for a local group.",
			},
		},
	}
}

func (d *groupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *groupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if !clientReady(d.client, &resp.Diagnostics) {
		return
	}

	var config groupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var doc groupDoc
	err := d.client.groups().FindOne(ctx, bson.M{"name": config.Name.ValueString()}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		resp.Diagnostics.AddError("No such group", fmt.Sprintf(
			"Database %q has no group named %q.", d.client.DatabaseName(), config.Name.ValueString()))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Cannot look up the group", err.Error())
		return
	}

	config.ID = types.StringValue(doc.ID.Hex())
	config.Description = optionalString(doc.Description)
	config.Source = types.StringValue(doc.Source)
	config.MemberIDs = stringSet(ctx, doc.MemberIDs, &resp.Diagnostics)

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
