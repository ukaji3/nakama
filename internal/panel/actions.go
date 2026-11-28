package panel

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/ukaji3/nakama/internal/service"
)

// The operations the panel starts (ADR 0031 decisions 5–7): each turns
// the posted form into the CLI command line of the matching
// `nakama aws …` command. The CLI validates every value as it does on
// the command line; this file keeps the command line well formed:
// positional values are checked (instance names, addresses) and placed
// after "--" where needed, and free values always ride in --flag=value
// form, so a value can never become a flag.

// confirmKind is how an operation is confirmed before it starts.
type confirmKind int

const (
	confirmNone confirmKind = iota
	// confirmDialog asks once (stops, charges).
	confirmDialog
	// confirmTyped requires typing the target's name (cannot be
	// undone).
	confirmTyped
)

// plannedOp is an operation built from a form.
type plannedOp struct {
	action    string
	args      []string
	instance  string
	exclusive bool
	confirm   confirmKind
	// typed is what confirmTyped requires the user to type.
	typed string
	// title is shown in the operation list ("create dev01").
	title string
}

// errInvalidInput is a form value the panel refuses before running.
type errInvalidInput struct{ field, reason string }

func (e *errInvalidInput) Error() string { return e.field + ": " + e.reason }

var (
	instanceTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,30}$`)
	versionPattern      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	agePattern          = regexp.MustCompile(`^[0-9]{1,6}[a-z]{0,2}$`)
)

func formName(f url.Values) (string, error) {
	name := strings.TrimSpace(f.Get("name"))
	if err := service.ValidateInstanceName(name); err != nil {
		return "", &errInvalidInput{"name", err.Error()}
	}
	return name, nil
}

// text returns a free text value without control characters.
func text(f url.Values, field string, maxLen int) (string, error) {
	v := strings.TrimSpace(f.Get(field))
	if len(v) > maxLen {
		return "", &errInvalidInput{field, fmt.Sprintf("longer than %d characters", maxLen)}
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", &errInvalidInput{field, "contains a control character"}
		}
	}
	return v, nil
}

func matching(f url.Values, field string, re *regexp.Regexp, required bool) (string, error) {
	v := strings.TrimSpace(f.Get(field))
	if v == "" {
		if required {
			return "", &errInvalidInput{field, "required"}
		}
		return "", nil
	}
	if !re.MatchString(v) {
		return "", &errInvalidInput{field, "not a valid value"}
	}
	return v, nil
}

func number(f url.Values, field string, lo, hi int, required bool) (string, error) {
	v := strings.TrimSpace(f.Get(field))
	if v == "" {
		if required {
			return "", &errInvalidInput{field, "required"}
		}
		return "", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return "", &errInvalidInput{field, fmt.Sprintf("give a whole number from %d to %d", lo, hi)}
	}
	return strconv.Itoa(n), nil
}

func checked(f url.Values, field string) bool { return f.Get(field) == "1" }

// ipv4 returns an IPv4 address or CIDR in canonical form.
func ipv4(f url.Values, field string, required bool) (string, error) {
	v := strings.TrimSpace(f.Get(field))
	if v == "" {
		if required {
			return "", &errInvalidInput{field, "required"}
		}
		return "", nil
	}
	if p, err := netip.ParsePrefix(v); err == nil && p.Addr().Is4() {
		return p.Masked().String(), nil
	}
	if a, err := netip.ParseAddr(v); err == nil && a.Is4() {
		return a.String(), nil
	}
	return "", &errInvalidInput{field, "not an IPv4 address or CIDR block"}
}

// flagValue appends --flag=value when value is set.
func flagValue(args []string, flag, value string) []string {
	if value == "" {
		return args
	}
	return append(args, "--"+flag+"="+value)
}

func flagIf(args []string, flag string, on bool) []string {
	if !on {
		return args
	}
	return append(args, "--"+flag)
}

// provisionFlags are the provisioning flags create and provision share.
func provisionFlags(args []string, f url.Values) ([]string, error) {
	setup, err := text(f, "setup", 2048)
	if err != nil {
		return nil, err
	}
	dotfiles, err := text(f, "dotfiles", 2048)
	if err != nil {
		return nil, err
	}
	preset := f.Get("preset")
	if preset != "" && preset != "kiro" {
		return nil, &errInvalidInput{"preset", "unknown preset"}
	}
	args = flagValue(args, "setup", setup)
	args = flagValue(args, "dotfiles", dotfiles)
	return flagValue(args, "preset", preset), nil
}

// roleFlags are --with-admin / --with-role (create, restore).
func roleFlags(args []string, f url.Values) ([]string, error) {
	role, err := text(f, "with_role", 128)
	if err != nil {
		return nil, err
	}
	args = flagIf(args, "with-admin", checked(f, "with_admin"))
	return flagValue(args, "with-role", role), nil
}

// planOperation turns a posted form into an operation.
func planOperation(f url.Values) (*plannedOp, error) {
	action := f.Get("action")
	op := &plannedOp{action: action}
	var err error
	needName := func() bool {
		var name string
		name, err = formName(f)
		op.instance = name
		return err == nil
	}
	switch action {
	case "start", "stop":
		if !needName() {
			return nil, err
		}
		op.args = []string{"aws", action, op.instance}
		if action == "stop" {
			op.confirm = confirmDialog
		}
	case "create":
		if !needName() {
			return nil, err
		}
		it, err := matching(f, "instance_type", instanceTypePattern, true)
		if err != nil {
			return nil, err
		}
		csv, err := matching(f, "code_server_version", versionPattern, false)
		if err != nil {
			return nil, err
		}
		args := []string{"aws", "create", op.instance, "--instance-type=" + it}
		args = flagIf(args, "bare", checked(f, "bare"))
		args = flagIf(args, "cloudfront", checked(f, "cloudfront"))
		args = flagIf(args, "no-kiro", checked(f, "no_kiro"))
		args = flagValue(args, "code-server-version", csv)
		if args, err = provisionFlags(args, f); err != nil {
			return nil, err
		}
		if args, err = roleFlags(args, f); err != nil {
			return nil, err
		}
		op.args = args
	case "delete":
		if !needName() {
			return nil, err
		}
		op.args = flagIf([]string{"aws", "delete", op.instance, "--yes"}, "force", checked(f, "force"))
		op.confirm, op.typed = confirmTyped, op.instance
	case "edit_type":
		if !needName() {
			return nil, err
		}
		it, err := matching(f, "instance_type", instanceTypePattern, true)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "edit", op.instance, "--instance-type=" + it, "--yes"}
		op.confirm = confirmDialog
	case "edit_ebs":
		if !needName() {
			return nil, err
		}
		size, err := number(f, "ebs", 1, 65536, true)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "edit", op.instance, "--ebs=" + size, "--yes"}
		op.confirm = confirmDialog
	case "update":
		if !needName() {
			return nil, err
		}
		keep, err := number(f, "keep", 0, 10000, false)
		if err != nil {
			return nil, err
		}
		op.args = flagValue([]string{"aws", "update", op.instance, "--yes"}, "keep", keep)
		op.confirm = confirmDialog
	case "provision":
		if !needName() {
			return nil, err
		}
		args, err := provisionFlags([]string{"aws", "provision", op.instance}, f)
		if err != nil {
			return nil, err
		}
		op.args = args
	case "backup_create":
		if !needName() {
			return nil, err
		}
		label, err := text(f, "label", 64)
		if err != nil {
			return nil, err
		}
		note, err := text(f, "note", 255)
		if err != nil {
			return nil, err
		}
		keep, err := number(f, "keep", 0, 10000, false)
		if err != nil {
			return nil, err
		}
		args := flagValue([]string{"aws", "backup", "create", op.instance}, "label", label)
		args = flagValue(args, "note", note)
		op.args = flagValue(args, "keep", keep)
	case "backup_note":
		if !needName() {
			return nil, err
		}
		label, err := text(f, "label", 64)
		if err != nil || label == "" {
			return nil, &errInvalidInput{"label", "required"}
		}
		note, err := text(f, "note", 255)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "backup", "note", op.instance, "--label=" + label, "--note=" + note}
	case "backup_delete":
		if !needName() {
			return nil, err
		}
		label, err := text(f, "label", 64)
		if err != nil || label == "" {
			return nil, &errInvalidInput{"label", "required"}
		}
		op.args = []string{"aws", "backup", "delete", op.instance, "--label=" + label, "--yes"}
		op.confirm, op.typed = confirmTyped, label
	case "restore":
		if !needName() {
			return nil, err
		}
		label, err := text(f, "label", 64)
		if err != nil {
			return nil, err
		}
		it, err := matching(f, "instance_type", instanceTypePattern, false)
		if err != nil {
			return nil, err
		}
		args := flagValue([]string{"aws", "restore", op.instance}, "label", label)
		args = flagValue(args, "instance-type", it)
		args = flagIf(args, "bare", checked(f, "bare"))
		args = flagIf(args, "cloudfront", checked(f, "cloudfront"))
		if args, err = roleFlags(args, f); err != nil {
			return nil, err
		}
		op.args = append(args, "--yes")
		op.confirm, op.typed = confirmTyped, op.instance
	case "cloudfront_add", "cloudfront_delete":
		if !needName() {
			return nil, err
		}
		verb := strings.TrimPrefix(action, "cloudfront_")
		args := flagIf([]string{"aws", "cloudfront", verb, op.instance}, "kirocrew", checked(f, "kirocrew"))
		if verb == "delete" {
			args = append(args, "--yes")
		}
		op.args = args
		op.confirm = confirmDialog
	case "acl_add":
		cidr, err := ipv4(f, "cidr", false)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "acl", "add"}
		if cidr != "" {
			op.args = append(op.args, "--", cidr)
		}
		op.instance = "" // the allowlist is shared
	case "acl_delete":
		cidr, err := ipv4(f, "cidr", true)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "acl", "delete", "--yes", "--", cidr}
		op.confirm = confirmDialog
	case "acl_delete_all":
		op.args = flagIf([]string{"aws", "acl", "delete", "all", "--yes"}, "keep-mine", checked(f, "keep_mine"))
		op.confirm, op.typed = confirmTyped, "all"
	case "acl_prune":
		age, err := matching(f, "older_than", agePattern, true)
		if err != nil {
			return nil, err
		}
		op.args = []string{"aws", "acl", "prune", "--older-than=" + age, "--yes"}
		op.confirm, op.typed = confirmTyped, "prune"
	case "init":
		nat := f.Get("nat")
		if nat != "" && nat != "gateway" && nat != "instance" {
			return nil, &errInvalidInput{"nat", "give gateway or instance"}
		}
		op.args = flagValue([]string{"aws", "init", "--yes"}, "nat", nat)
		op.exclusive, op.confirm = true, confirmDialog
	case "tunnel_stop", "tunnel_restart":
		if !needName() {
			return nil, err
		}
		op.args = []string{"aws", "tunnel", strings.TrimPrefix(action, "tunnel_"), op.instance}
	default:
		return nil, errors.New("unknown operation")
	}
	op.args = append(op.args, "-o", "json")
	op.title = strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(action, ""), "_", " ") + " " + op.instance)
	return op, nil
}

// commandLine renders the operation as the user would type it.
func commandLine(args []string) string {
	parts := []string{"nakama"}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$`") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}
