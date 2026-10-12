package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"
	smithy "github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakeCreateWAF answers CreateWebACL from a script of errors: the i-th
// call returns errs[i] (nil or beyond the slice => success). The list
// calls report no existing resources.
type fakeCreateWAF struct {
	awsx.WAFAPI

	mu       sync.Mutex
	errs     []error
	calls    int
	lastName string
	// onCall, when set, runs before each CreateWebACL returns.
	onCall func(call int)
}

func (f *fakeCreateWAF) ListWebACLs(context.Context, *wafv2.ListWebACLsInput, ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error) {
	return &wafv2.ListWebACLsOutput{}, nil
}

func (f *fakeCreateWAF) CreateWebACL(_ context.Context, in *wafv2.CreateWebACLInput, _ ...func(*wafv2.Options)) (*wafv2.CreateWebACLOutput, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.lastName = aws.ToString(in.Name)
	cb := f.onCall
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	f.mu.Unlock()
	if cb != nil {
		cb(i + 1)
	}
	if err != nil {
		return nil, err
	}
	return &wafv2.CreateWebACLOutput{Summary: &wafv2types.WebACLSummary{
		Name: in.Name, Id: aws.String("acl-id"), ARN: aws.String("arn:aws:wafv2:us-east-1:111122223333:global/webacl/nakama-waf/acl-id"),
	}}, nil
}

func (f *fakeCreateWAF) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func wafErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "AWS WAF couldn’t retrieve the resource that you requested. Retry your request."}
}

// shortWAFPropagation shrinks the retry pacing for the test (not
// parallel: it changes package variables).
func shortWAFPropagation(t *testing.T, interval, maxWait time.Duration) {
	t.Helper()
	prevI, prevM := wafPropagationInterval, wafPropagationMaxWait
	wafPropagationInterval, wafPropagationMaxWait = interval, maxWait
	t.Cleanup(func() { wafPropagationInterval, wafPropagationMaxWait = prevI, prevM })
}

const testIPSetARN = "arn:aws:wafv2:us-east-1:111122223333:global/ipset/nakama-allowed-ips/ipset-id"

func TestUpsertWebACLWaitsForTheIPSetToPropagate(t *testing.T) {
	shortWAFPropagation(t, time.Millisecond, time.Second)
	// Listed here rather than taken from wafPropagationErrorCodes, so that
	// dropping a code from the implementation fails this test. Both are
	// documented for CreateWebACL as "you might just need to wait".
	for _, code := range []string{"WAFUnavailableEntityException", "WAFNonexistentItemException"} {
		t.Run(code, func(t *testing.T) {
			fake := &fakeCreateWAF{errs: []error{wafErr(code), wafErr(code), wafErr(code)}}
			svc := &Service{clients: &awsx.Clients{WAF: fake}}
			id, arn, err := svc.upsertWebACL(context.Background(), testIPSetARN)
			if err != nil {
				t.Fatalf("upsertWebACL: %v", err)
			}
			if id != "acl-id" || !strings.HasSuffix(arn, "/acl-id") {
				t.Errorf("got id=%q arn=%q", id, arn)
			}
			if n := fake.callCount(); n != 4 {
				t.Errorf("CreateWebACL calls = %d, want 4 (3 propagation errors, then success)", n)
			}
		})
	}
}

func TestUpsertWebACLGivesUpAfterTheMaxWait(t *testing.T) {
	shortWAFPropagation(t, 5*time.Millisecond, 40*time.Millisecond)
	errs := make([]error, 1000)
	for i := range errs {
		errs[i] = wafErr("WAFUnavailableEntityException")
	}
	fake := &fakeCreateWAF{errs: errs}
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	start := time.Now()
	_, _, err := svc.upsertWebACL(context.Background(), testIPSetARN)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("upsertWebACL must fail once the max wait is exhausted")
	}
	if !strings.Contains(err.Error(), "WAFUnavailableEntityException") || !strings.Contains(err.Error(), "nakama aws init") {
		t.Errorf("the error must keep the AWS error and say how to recover: %v", err)
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "WAFUnavailableEntityException" {
		t.Errorf("the AWS error must stay unwrappable: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("gave up after %v; the max wait is 40ms", elapsed)
	}
	if n := fake.callCount(); n < 2 || n > 20 {
		t.Errorf("CreateWebACL calls = %d, want a handful within the max wait", n)
	}
}

func TestUpsertWebACLDoesNotRetryOtherErrors(t *testing.T) {
	shortWAFPropagation(t, time.Millisecond, time.Second)
	for _, code := range []string{"AccessDeniedException", "WAFInvalidParameterException", "WAFLimitsExceededException", "WAFDuplicateItemException"} {
		t.Run(code, func(t *testing.T) {
			fake := &fakeCreateWAF{errs: []error{wafErr(code)}}
			svc := &Service{clients: &awsx.Clients{WAF: fake}}
			_, _, err := svc.upsertWebACL(context.Background(), testIPSetARN)
			if err == nil || !strings.Contains(err.Error(), code) {
				t.Fatalf("want the %s error at once, got %v", code, err)
			}
			if n := fake.callCount(); n != 1 {
				t.Errorf("CreateWebACL calls = %d, want 1 (no retry for %s)", n, code)
			}
		})
	}
	// A non-API error (network, credentials) is not retried either.
	fake := &fakeCreateWAF{errs: []error{errors.New("dial tcp: i/o timeout")}}
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	if _, _, err := svc.upsertWebACL(context.Background(), testIPSetARN); err == nil || fake.callCount() != 1 {
		t.Errorf("err=%v calls=%d, want one call and the error", err, fake.callCount())
	}
}

func TestUpsertWebACLStopsWaitingWhenCancelled(t *testing.T) {
	shortWAFPropagation(t, time.Hour, 2*time.Hour) // would wait an hour
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeCreateWAF{errs: []error{wafErr("WAFUnavailableEntityException")}}
	fake.onCall = func(int) { cancel() } // cancelled during the first wait
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	done := make(chan error, 1)
	go func() {
		_, _, err := svc.upsertWebACL(ctx, testIPSetARN)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upsertWebACL kept waiting after the context was cancelled")
	}
}

func TestUpsertWebACLSucceedsFirstTimeWithoutWaiting(t *testing.T) {
	shortWAFPropagation(t, time.Hour, 2*time.Hour)
	fake := &fakeCreateWAF{}
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	start := time.Now()
	if _, _, err := svc.upsertWebACL(context.Background(), testIPSetARN); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second || fake.callCount() != 1 {
		t.Errorf("a first-time success must not wait (calls=%d)", fake.callCount())
	}
	if fake.lastName != "nakama-waf" {
		t.Errorf("created web ACL name = %q", fake.lastName)
	}
}
