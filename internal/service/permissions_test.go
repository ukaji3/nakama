package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sdkCallPattern extracts AWS SDK calls made through the awsx client
// set, in both spellings used by this package (s.clients.EC2.…,
// opts.Clients.IAM.…).
var sdkCallPattern = regexp.MustCompile(
	`\.[cC]lients\.(EC2|SSM|CloudFront|WAF|IAM|STS)\.([A-Z][A-Za-z]*)\(`)

// iamPrefixOf maps an awsx client field name to its IAM action prefix.
var iamPrefixOf = map[string]string{
	"EC2":        "ec2",
	"SSM":        "ssm",
	"CloudFront": "cloudfront",
	"WAF":        "wafv2",
	"IAM":        "iam",
	"STS":        "sts",
}

// extractedIAMActions scans this package's non-test sources and returns
// the IAM actions implied by the SDK calls, as a set.
func extractedIAMActions(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	actions := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range sdkCallPattern.FindAllStringSubmatch(string(src), -1) {
			actions[iamPrefixOf[m[1]]+":"+m[2]] = true
		}
	}
	if len(actions) == 0 {
		t.Fatal("extracted no SDK calls; the extraction pattern is broken")
	}
	return actions
}

// matrixActionSet returns every action in the matrix (all rows,
// including non-simulated ones), as a set.
func matrixActionSet() map[string]bool {
	set := map[string]bool{}
	for _, row := range featurePermissionMatrix() {
		for _, a := range row.Actions {
			set[a] = true
		}
	}
	return set
}

// TestPermissionMatrixCoversExtractedSDKCalls is the forward drift
// check: every SDK call in the implementation must be represented in
// the matrix. Adding an AWS call without updating the matrix fails CI.
func TestPermissionMatrixCoversExtractedSDKCalls(t *testing.T) {
	matrix := matrixActionSet()
	var missing []string
	for action := range extractedIAMActions(t) {
		if !matrix[action] {
			missing = append(missing, action)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the implementation calls AWS APIs whose actions are missing from "+
			"featurePermissionMatrix (internal/service/permissions.go); add them "+
			"to the feature(s) whose code path calls them:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestPermissionMatrixHasNoStaleActions is the reverse drift check:
// every matrix action must either be extracted from the implementation
// or be declared in implicitActions with a justification. A leftover
// action from removed code fails CI.
func TestPermissionMatrixHasNoStaleActions(t *testing.T) {
	extracted := extractedIAMActions(t)
	var stale []string
	for action := range matrixActionSet() {
		if !extracted[action] && implicitActions[action] == "" {
			stale = append(stale, action)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("featurePermissionMatrix lists actions that no code path calls and "+
			"that are not declared in implicitActions; remove them or declare why "+
			"they are needed:\n  %s", strings.Join(stale, "\n  "))
	}
}

// TestImplicitActionsAreNotExtracted keeps the implicit declarations
// honest: once an action becomes visible to extraction (e.g. the call
// moves into this package), its declaration must be removed.
func TestImplicitActionsAreNotExtracted(t *testing.T) {
	extracted := extractedIAMActions(t)
	for action, why := range implicitActions {
		if extracted[action] {
			t.Errorf("implicitActions declares %q (%s) but the action is now "+
				"extracted from the implementation; drop the stale declaration",
				action, why)
		}
	}
}

// TestImplicitActionsAreUsed prevents orphan declarations: every
// implicit action must actually appear in some matrix row.
func TestImplicitActionsAreUsed(t *testing.T) {
	matrix := matrixActionSet()
	for action := range implicitActions {
		if !matrix[action] {
			t.Errorf("implicitActions declares %q but no matrix row lists it", action)
		}
	}
}

// TestPermissionMatrixHygiene pins structural invariants: unique
// feature labels, non-empty sorted action lists, and well-formed action
// names.
func TestPermissionMatrixHygiene(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]+:[A-Za-z0-9]+$`)
	seen := map[string]bool{}
	for _, row := range featurePermissionMatrix() {
		if row.Feature == "" || row.Commands == "" {
			t.Errorf("row %+v: Feature and Commands must be non-empty", row)
		}
		if seen[row.Feature] {
			t.Errorf("duplicate feature label %q", row.Feature)
		}
		seen[row.Feature] = true
		if len(row.Actions) == 0 {
			t.Errorf("feature %q has no actions", row.Feature)
		}
		if !sort.StringsAreSorted(row.Actions) {
			t.Errorf("feature %q: actions are not sorted (mergeActions was bypassed)", row.Feature)
		}
		for i, a := range row.Actions {
			if !valid.MatchString(a) {
				t.Errorf("feature %q: malformed action %q", row.Feature, a)
			}
			if i > 0 && row.Actions[i-1] == a {
				t.Errorf("feature %q: duplicate action %q", row.Feature, a)
			}
		}
	}
}

// TestSimulatedActionUnion pins that the union doctor simulates is the
// union of exactly the Simulate rows.
func TestSimulatedActionUnion(t *testing.T) {
	union := simulatedActionUnion()
	if !sort.StringsAreSorted(union) {
		t.Error("simulatedActionUnion must be sorted")
	}
	inUnion := map[string]bool{}
	for _, a := range union {
		inUnion[a] = true
	}
	for _, row := range featurePermissionMatrix() {
		for _, a := range row.Actions {
			if row.Simulate && !inUnion[a] {
				t.Errorf("action %q of simulated feature %q is missing from the union", a, row.Feature)
			}
		}
	}
	for _, a := range union {
		if a == "sts:GetCallerIdentity" {
			t.Errorf("non-simulated action %q leaked into the union", a)
		}
	}
}

// permissionsDocPath locates docs/requirements.md relative to this
// package directory.
const permissionsDocPath = "../../docs/requirements.md"

// TestPermissionsDocsSection verifies that the generated block in
// docs/requirements.md matches the matrix. Run with
// NAKAMA_UPDATE_PERMISSIONS_DOCS=1 to rewrite the block in place.
func TestPermissionsDocsSection(t *testing.T) {
	want := renderPermissionsMarkdown()
	raw, err := os.ReadFile(filepath.Clean(permissionsDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", permissionsDocPath, err)
	}
	doc := string(raw)
	begin := strings.Index(doc, permissionsDocBegin)
	end := strings.Index(doc, permissionsDocEnd)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("%s does not contain the %s / %s marker pair",
			permissionsDocPath, permissionsDocBegin, permissionsDocEnd)
	}
	got := doc[begin : end+len(permissionsDocEnd)]
	if got == want {
		return
	}
	if os.Getenv("NAKAMA_UPDATE_PERMISSIONS_DOCS") == "1" {
		updated := doc[:begin] + want + doc[end+len(permissionsDocEnd):]
		if err := os.WriteFile(permissionsDocPath, []byte(updated), 0o644); err != nil {
			t.Fatalf("rewrite %s: %v", permissionsDocPath, err)
		}
		t.Logf("rewrote the generated permissions block in %s", permissionsDocPath)
		return
	}
	t.Errorf("the generated permissions block in %s is stale; regenerate it with:\n"+
		"  NAKAMA_UPDATE_PERMISSIONS_DOCS=1 go test ./internal/service/ -run TestPermissionsDocsSection\n"+
		"diff summary: docs block %d bytes, expected %d bytes",
		permissionsDocPath, len(got), len(want))
}

// TestRenderPermissionsMarkdownShape pins the rendered block structure
// so the docs table stays parseable.
func TestRenderPermissionsMarkdownShape(t *testing.T) {
	out := renderPermissionsMarkdown()
	if !strings.HasPrefix(out, permissionsDocBegin+"\n") {
		t.Error("rendered block must start with the begin marker")
	}
	if !strings.HasSuffix(out, permissionsDocEnd) {
		t.Error("rendered block must end with the end marker")
	}
	rows := featurePermissionMatrix()
	for _, row := range rows {
		if !strings.Contains(out, fmt.Sprintf("| %s |", row.Feature)) {
			t.Errorf("rendered block is missing the row for %q", row.Feature)
		}
	}
	// One table line per row plus header and separator.
	tableLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "|") {
			tableLines++
		}
	}
	if want := len(rows) + 2; tableLines != want {
		t.Errorf("rendered table has %d lines, want %d (rows + header + separator)",
			tableLines, want)
	}
}
