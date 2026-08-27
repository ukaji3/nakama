package service

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// TestDeleteInstanceAbortsWhenSSMFails pins the M4 fix: when the
// resource-ID reads fail for a reason other than ParameterNotFound
// (SSM outage, expired credentials), delete must abort with an error
// instead of treating every ID as absent and reporting success while
// CloudFront/EC2/ENI/SG survive.
func TestDeleteInstanceAbortsWhenSSMFails(t *testing.T) {
	t.Parallel()

	svc := &Service{clients: &awsx.Clients{
		// Only SSM is wired: DeleteInstance must fail before touching
		// any other client (a nil-pointer panic here would mean the
		// abort did not happen first).
		SSM: fakeSSMScripted{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}},
	}}
	err := svc.DeleteInstance(context.Background(), DeleteInstanceInput{Name: "dev01"})
	if err == nil {
		t.Fatal("DeleteInstance succeeded despite SSM failure")
	}
	if !strings.Contains(err.Error(), "GetParameter") {
		t.Errorf("error does not surface the failing read: %v", err)
	}
}
