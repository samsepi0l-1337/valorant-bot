package riot

import (
	"net/http"
	"testing"
)

func TestMFASchemaRejectionRequiresExplicitNonConsumingMarker(t *testing.T) {
	body := map[string]any{"type": "multifactor"}
	for _, raw := range []string{
		`{"type":"error","error":"invalid_request"}`,
		`{"type":"error","error":"invalid_request","error_description":"multifactor schema rejected"}`,
		`{"type":"error","error":"auth_failure","error_description":"multifactor request schema rejected before otp processing"}`,
	} {
		if isMFASchemaRejection(http.StatusBadRequest, body, []byte(raw)) {
			t.Fatalf("ambiguous response classified as non-consuming: %s", raw)
		}
	}
}
