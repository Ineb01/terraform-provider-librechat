package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment variables are read when the matching attribute is absent, so a MongoDB URI
// - which reaches a database holding every conversation in the deployment - never has to
// be written into a committed .tf file.
const (
	envMongoURI  = "LIBRECHAT_MONGO_URI"
	envMongoDB   = "LIBRECHAT_MONGO_DATABASE"
	providerName = "librechat"
)

var _ provider.Provider = (*librechatProvider)(nil)

type librechatProvider struct {
	version string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &librechatProvider{version: version}
	}
}

type providerModel struct {
	MongoURI types.String `tfsdk:"mongo_uri"`
	Database types.String `tfsdk:"database"`
}

func (p *librechatProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = providerName
	resp.Version = p.version
}

func (p *librechatProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages LibreChat users, agents, MCP servers, groups, roles and " +
			"permissions by writing the documents LibreChat reads. Requires network access to " +
			"LibreChat's MongoDB.",
		Attributes: map[string]schema.Attribute{
			"mongo_uri": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "MongoDB connection string for the LibreChat database, e.g. " +
					"`mongodb://127.0.0.1:27017/LibreChat`. Falls back to the `" + envMongoURI +
					"` environment variable. This grants full read/write access to every " +
					"conversation in the deployment, so prefer the environment variable or a " +
					"value read from a secret store over a literal in a committed file.",
			},
			"database": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Database name. Defaults to the path of `mongo_uri`, then to " +
					"`" + defaultDatabase + "`. Falls back to the `" + envMongoDB + "` environment variable.",
			},
		},
	}
}

func (p *librechatProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unknown means the value is derived from something not yet applied - a MongoDB container
	// created in the same run, say. Return without a diagnostic: OpenTofu calls Configure again
	// during apply, once the value is known, and erroring here would make it impossible to
	// build a LibreChat and populate it in one apply.
	//
	// The cost is that resources can be asked to do work before a client exists, which is what
	// clientReady guards against. An earlier version raised an error here instead, and the
	// symptom was a plan that refused to consider any librechat_* resource at all.
	if config.MongoURI.IsUnknown() || config.Database.IsUnknown() {
		return
	}

	uri := config.MongoURI.ValueString()
	if uri == "" {
		uri = os.Getenv(envMongoURI)
	}
	if uri == "" {
		resp.Diagnostics.AddAttributeError(
			pathRoot("mongo_uri"),
			"Missing MongoDB connection string",
			"Set the provider's mongo_uri attribute or the "+envMongoURI+" environment variable.",
		)
		return
	}

	database := config.Database.ValueString()
	if database == "" {
		database = os.Getenv(envMongoDB)
	}

	client, err := NewClient(ctx, uri, database)
	if err != nil {
		resp.Diagnostics.AddError(
			"Cannot reach LibreChat's MongoDB",
			"The provider connects to MongoDB directly; LibreChat's REST API has no endpoints "+
				"for roles, groups or permissions.\n\n"+
				err.Error()+"\n\n"+
				"Note that LibreChat's MongoDB is usually only reachable from the Docker network "+
				"it runs on. Reaching it from a workstation generally needs an SSH tunnel to the "+
				"daemon host.",
		)
		return
	}

	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *librechatProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewUserResource,
		NewGroupResource,
		NewAgentResource,
		NewMCPServerResource,
		NewRoleResource,
		NewRolePermissionsResource,
		NewGrantResource,
	}
}

func (p *librechatProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewUserDataSource,
		NewGroupDataSource,
		NewAccessRoleDataSource,
	}
}
