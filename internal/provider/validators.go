package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// diagnosticsSink lets helper methods take diagnostics without every signature naming the
// framework's diag package.
type diagnosticsSink = diag.Diagnostics

// The same shape LibreChat's user schema matches on (/\S+@\S+\.\S+/), tightened only by
// also rejecting a second @.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// An absolute http(s) URL. Deliberately loose about the rest of the URL: it is resolved by
// LibreChat inside its own container, so what matters here is only that a scheme and a host
// are present - a relative path would be silently unreachable.
var urlPattern = regexp.MustCompile(`^https?://[^/\s]+`)

// bcrypt's modular crypt format. $2$ and $2x$ exist historically but are broken variants;
// bcryptjs will not produce them and there is no reason to accept them here.
var bcryptPattern = regexp.MustCompile(`^\$2[aby]\$\d{2}\$`)

// lowercaseValidator rejects a value with any uppercase in it.
//
// Why this is a hard error rather than a silent normalisation: LibreChat's user schema
// declares `lowercase: true` on email and username, so mongoose lowercases both when the
// application writes or queries them. This provider writes with the raw driver, which
// applies no such setter. Normalising quietly would put a value in the database that differs
// from the one in the configuration, and Terraform reports that as "provider produced
// inconsistent result after apply". Refusing up front says what to change instead.
type lowercaseValidator struct {
	field string
}

var _ validator.String = lowercaseValidator{}

func (v lowercaseValidator) Description(_ context.Context) string {
	return "must be lowercase"
}

func (v lowercaseValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v lowercaseValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	lowered := strings.ToLower(value)
	if value == lowered {
		return
	}

	resp.Diagnostics.AddAttributeError(
		req.Path,
		"Must be lowercase",
		fmt.Sprintf("LibreChat's schema lowercases %s, so %q would be stored as %q and the two "+
			"would stop matching. Write %q.", v.field, value, lowered, lowered),
	)
}
