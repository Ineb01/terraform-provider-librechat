// A Terraform/OpenTofu provider for LibreChat.
//
// It talks to LibreChat's MongoDB directly rather than to its REST API. That is not a
// shortcut: the API has no endpoints for roles, groups or role permissions at all, and
// librechat.yaml can only write the USE bit of a permission type - so the database is the
// only surface that covers users, agents, MCP servers, groups, roles AND permissions.
// Every collection this provider writes is documented in internal/provider/client.go
// against the schemas shipped in the LibreChat image.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/Ineb01/terraform-provider-librechat/internal/provider"
)

// Overwritten at link time by build.ps1 (-ldflags). Only surfaces in the provider's
// user agent and in `tofu providers`, so "dev" is a fine default for a dev_overrides build.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	// The plugin's own identity, used in the protocol handshake and by -debug. It is
	// conventionally the Terraform Registry form even for a provider used through OpenTofu:
	// the address here does not decide where the provider is fetched from. `source =
	// "ineb01/librechat"` resolves against whichever default registry the CLI has -
	// registry.terraform.io for Terraform, registry.opentofu.org for OpenTofu - and both serve
	// the same GitHub release.
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/ineb01/librechat",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
