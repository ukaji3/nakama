package cli

import (
	"strings"
	"testing"

	"github.com/ukaji3/nakama/internal/service"
)

func TestFormatInstanceTable_Empty(t *testing.T) {
	if got := formatInstanceTable(nil); got != "(no nakama instances)" {
		t.Fatalf("empty table = %q", got)
	}
}

func TestFormatInstanceTable_HeaderAndRows(t *testing.T) {
	rows := []service.InstanceSummary{
		{Name: "dev01", Type: "vscode", InstanceID: "i-1", State: "running", InstanceType: "t3.medium", DistributionDomain: "d1.cloudfront.net"},
		{Name: "nakama-nat", Type: "nat", InstanceID: "i-nat", State: "stopped", InstanceType: "t4g.nano"},
	}
	out := formatInstanceTable(rows)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), out)
	}

	header := lines[0]
	for _, col := range []string{"NAME", "INSTANCE_ID", "STATE", "TYPE", "INSTANCE_TYPE", "DISTRIBUTION_DOMAIN"} {
		if !strings.Contains(header, col) {
			t.Fatalf("header missing column %q:\n%s", col, header)
		}
	}

	// TYPE column is rendered uppercase (VSCODE / NAT).
	if !strings.Contains(lines[1], "VSCODE") {
		t.Fatalf("vscode row missing VSCODE type:\n%s", lines[1])
	}
	if !strings.Contains(lines[1], "t3.medium") || !strings.Contains(lines[1], "d1.cloudfront.net") {
		t.Fatalf("vscode row missing instance-type/domain:\n%s", lines[1])
	}
	if !strings.Contains(lines[2], "NAT") || !strings.Contains(lines[2], "t4g.nano") {
		t.Fatalf("nat row missing NAT/instance-type:\n%s", lines[2])
	}
}

func TestFormatInstanceTable_NATRowsShowTheEgressIP(t *testing.T) {
	rows := []service.InstanceSummary{
		{Name: "dev01", Type: "vscode", InstanceID: "i-1", State: "running", InstanceType: "t3.medium", DistributionDomain: "d1.cloudfront.net"},
		{Name: "nakama-natgw", Type: "natgw", InstanceID: "nat-0123456789abcdef0", State: "available", PublicIP: "203.0.113.10"},
	}
	lines := strings.Split(formatInstanceTable(rows), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d", len(lines))
	}
	for _, want := range []string{"NATGW", "nat-0123456789abcdef0", "available", "egress 203.0.113.10"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("natgw row lacks %q:\n%s", want, lines[2])
		}
	}
	if strings.Contains(lines[1], "egress") {
		t.Errorf("a code-server row shows its distribution, not an egress IP:\n%s", lines[1])
	}
	// The columns stay aligned for the 21-character NAT gateway ID.
	if strings.Index(lines[0], "STATE") != strings.Index(lines[2], "available") {
		t.Errorf("STATE column misaligned:\n%s\n%s", lines[0], lines[2])
	}
}
