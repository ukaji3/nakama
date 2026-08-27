package service

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/wafv2"
	wafv2types "github.com/aws/aws-sdk-go-v2/service/wafv2/types"

	"github.com/ukaji3/nakama/internal/awsx"
)

// fakePagedWAF scripts paged ListIPSets/ListWebACLs responses keyed by
// the incoming NextMarker ("" for the first page).
type fakePagedWAF struct {
	awsx.WAFAPI
	ipPages  map[string]*wafv2.ListIPSetsOutput
	aclPages map[string]*wafv2.ListWebACLsOutput
	err      error
}

func markerKey(m *string) string {
	if m == nil {
		return ""
	}
	return *m
}

func (f *fakePagedWAF) ListIPSets(_ context.Context, in *wafv2.ListIPSetsInput, _ ...func(*wafv2.Options)) (*wafv2.ListIPSetsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	out, ok := f.ipPages[markerKey(in.NextMarker)]
	if !ok {
		return &wafv2.ListIPSetsOutput{}, nil
	}
	return out, nil
}

func (f *fakePagedWAF) ListWebACLs(_ context.Context, in *wafv2.ListWebACLsInput, _ ...func(*wafv2.Options)) (*wafv2.ListWebACLsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	out, ok := f.aclPages[markerKey(in.NextMarker)]
	if !ok {
		return &wafv2.ListWebACLsOutput{}, nil
	}
	return out, nil
}

func ipSummary(name string) wafv2types.IPSetSummary {
	return wafv2types.IPSetSummary{Name: aws.String(name), Id: aws.String("id-" + name), ARN: aws.String("arn-" + name)}
}

func aclSummary(name string) wafv2types.WebACLSummary {
	return wafv2types.WebACLSummary{Name: aws.String(name), Id: aws.String("id-" + name), ARN: aws.String("arn-" + name)}
}

// TestListAllIPSetsPaginates pins the L5 fix: resources beyond the
// first page are found, and a server echoing the same marker back
// does not loop forever.
func TestListAllIPSetsPaginates(t *testing.T) {
	t.Parallel()

	fake := &fakePagedWAF{ipPages: map[string]*wafv2.ListIPSetsOutput{
		"":   {IPSets: []wafv2types.IPSetSummary{ipSummary("a")}, NextMarker: aws.String("m1")},
		"m1": {IPSets: []wafv2types.IPSetSummary{ipSummary("nakama-allowed-ips")}, NextMarker: aws.String("m1")}, // echoed marker
	}}
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	all, err := svc.listAllIPSets(context.Background())
	if err != nil {
		t.Fatalf("listAllIPSets: %v", err)
	}
	if len(all) != 2 || *all[1].Name != "nakama-allowed-ips" {
		t.Errorf("all = %+v, want 2 entries ending with nakama-allowed-ips", all)
	}

	// findIPSetSummary must therefore locate a second-page entry.
	got, err := svc.findIPSetSummary(context.Background())
	if err != nil {
		t.Fatalf("findIPSetSummary: %v", err)
	}
	if got.id != "id-nakama-allowed-ips" {
		t.Errorf("findIPSetSummary = %+v", got)
	}
}

func TestListAllWebACLsPaginates(t *testing.T) {
	t.Parallel()

	fake := &fakePagedWAF{aclPages: map[string]*wafv2.ListWebACLsOutput{
		"":   {WebACLs: []wafv2types.WebACLSummary{aclSummary("other")}, NextMarker: aws.String("p2")},
		"p2": {WebACLs: []wafv2types.WebACLSummary{aclSummary("nakama-waf")}}, // no marker: last page
	}}
	svc := &Service{clients: &awsx.Clients{WAF: fake}}
	arn, err := svc.findWAFWebACLARN(context.Background())
	if err != nil {
		t.Fatalf("findWAFWebACLARN: %v", err)
	}
	if arn != "arn-nakama-waf" {
		t.Errorf("arn = %q", arn)
	}
}

func TestListAllIPSetsPropagatesError(t *testing.T) {
	t.Parallel()

	svc := &Service{clients: &awsx.Clients{WAF: &fakePagedWAF{err: errors.New("throttled")}}}
	if _, err := svc.listAllIPSets(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
