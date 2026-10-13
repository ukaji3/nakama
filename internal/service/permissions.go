package service

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the single source of truth for "which IAM actions does
// each nakama feature need". Three consumers derive from it:
//
//   - `nakama aws doctor` simulates the union of all actions with
//     iam:SimulatePrincipalPolicy and reports one capability line per
//     feature (see diagnoseCapabilities in doctor.go).
//   - The permissions appendix in docs/requirements.md is a generated
//     block rendered by renderPermissionsMarkdown; a test fails when
//     the docs drift from this table.
//   - Tests in permissions_test.go extract the SDK calls from this
//     package's sources and fail in BOTH directions: an extracted call
//     whose action is missing here (the table went stale against the
//     implementation), and a table action that is neither extracted
//     nor declared in implicitActions (the table lists a leftover).
//
// Actions that never appear as SDK method calls (they are demanded by
// AWS as a side effect, or called inside vendored code) must be
// declared in implicitActions with a justification.

// FeaturePermission ties one user-facing nakama capability to the IAM
// actions its code paths call.
type FeaturePermission struct {
	// Feature is the capability label shown by doctor and in docs
	// (e.g. "create").
	Feature string
	// Commands names the CLI commands the row covers, for the docs
	// table.
	Commands string
	// Actions are the IAM actions the feature's code paths require,
	// sorted and de-duplicated by featurePermissionMatrix.
	Actions []string
	// Note carries caveats (conditional flags, graceful degradation).
	Note string
	// Simulate marks the row for doctor's SimulatePrincipalPolicy
	// check. Rows with actions that cannot be meaningfully simulated
	// (e.g. sts:GetCallerIdentity, which needs no permissions) set it
	// to false and appear in the docs table only.
	Simulate bool
}

// Shared action sets. Each mirrors a helper that several features call,
// so per-feature rows stay honest about transitive requirements.
var (
	// resolveInstanceActions: name -> instance-id resolution plus the
	// SSM meta/parameter reads virtually every instance-scoped command
	// performs (instance.go, instance_query.go).
	resolveInstanceActions = []string{
		"ec2:DescribeInstances",
		"ssm:GetParameter",
	}

	// sessionActions: SSM sessions as used by connect/ssh/tunnel.
	// ssm:ResumeSession is called inside the vendored
	// session-manager-plugin, not via awsx (see implicitActions).
	sessionActions = []string{
		"ssm:StartSession",
		"ssm:TerminateSession",
		"ssm:ResumeSession",
	}

	// remoteScriptActions: runRemoteScript + waitSSMCommandOutput
	// (ssh.go, instance_update.go), shared by ssh setup, provisioning,
	// tunnel suggest, update and edit.
	remoteScriptActions = []string{
		"ssm:SendCommand",
		"ssm:GetCommandInvocation",
	}

	// natLifecycleActions: the NAT instance power-state reconciliation
	// (nat_instance.go). Every command that can start, stop, create or
	// delete a code-server instance calls it, and it may CREATE the
	// NAT instance on first use — hence the run/create set, the
	// tag-on-create and the PassRole for the NAT instance profile.
	natLifecycleActions = []string{
		"ec2:AuthorizeSecurityGroupIngress",
		"ec2:CreateSecurityGroup",
		"ec2:CreateTags",
		"ec2:DescribeInstances",
		"ec2:DescribeSecurityGroups",
		"ec2:ModifyInstanceAttribute",
		"ec2:RunInstances",
		"ec2:StartInstances",
		"ec2:StopInstances",
		"iam:PassRole",
		"ssm:GetParameter",
	}

	// backupActions: snapshot create/list/tag plus the root-volume
	// lookup (backup.go, rootVolumeOf in instance_edit.go). `update`
	// includes it because it takes an automatic pre-update backup.
	backupActions = []string{
		"ec2:CreateSnapshot",
		"ec2:CreateTags",
		"ec2:DeleteSnapshot",
		"ec2:DeleteTags",
		"ec2:DescribeSnapshots",
		"ec2:DescribeVolumes",
	}
)

// implicitActions declares the actions that legitimately appear in the
// matrix without a matching SDK call in this package. The reverse drift
// test fails on any matrix action that is neither extracted nor listed
// here, and a companion test fails when an entry becomes extractable
// (the declaration went stale).
var implicitActions = map[string]string{
	"ssm:ResumeSession": "called inside the vendored session-manager-plugin " +
		"(third_party/session-manager-plugin), not via the awsx clients",
	"iam:PassRole": "demanded by ec2:RunInstances when an instance profile is " +
		"attached (instance_create_ec2.go, nat_instance.go); there is no " +
		"SDK method of this name",
	"sts:GetCallerIdentity": "called directly in internal/awsx (probeIdentity), " +
		"outside this package's clients pattern",
}

// featurePermissionMatrix returns the feature-to-actions table with
// each row's actions sorted and de-duplicated. Row order is the
// display order used by doctor and the docs.
func featurePermissionMatrix() []FeaturePermission {
	rows := []FeaturePermission{
		{
			Feature:  "all commands",
			Commands: "資格情報を使う全コマンド",
			Actions:  []string{"sts:GetCallerIdentity"},
			Note: "資格情報の疎通確認。IAM ポリシーで拒否できない操作のため " +
				"doctor のシミュレーション対象外",
			Simulate: false,
		},
		{
			Feature:  "init",
			Commands: "nakama aws init",
			Actions: mergeActions(
				[]string{
					"ec2:AssociateRouteTable",
					"ec2:AttachInternetGateway",
					"ec2:CreateInternetGateway",
					"ec2:CreateRoute",
					"ec2:CreateRouteTable",
					"ec2:CreateSubnet",
					"ec2:CreateTags",
					"ec2:CreateVpc",
					"ec2:DeleteNatGateway",
					"ec2:DescribeAddresses",
					"ec2:DescribeAvailabilityZones",
					"ec2:DescribeInternetGateways",
					"ec2:DescribeNatGateways",
					"ec2:DescribeRouteTables",
					"ec2:DescribeSubnets",
					"ec2:DescribeVpcs",
					"ec2:ModifySubnetAttribute",
					"ec2:ModifyVpcAttribute",
					"ec2:ReleaseAddress",
					"ec2:ReplaceRoute",
					"iam:AddRoleToInstanceProfile",
					"iam:AttachRolePolicy",
					"iam:CreateInstanceProfile",
					"iam:CreateRole",
					"iam:GetInstanceProfile",
					"iam:GetRole",
					"iam:PutRolePolicy",
					"ssm:PutParameter",
					"wafv2:CreateIPSet",
					"wafv2:CreateWebACL",
					"wafv2:ListIPSets",
					"wafv2:ListWebACLs",
				},
				// The NAT instance is created here, and started for
				// the forwarding check when it is stopped.
				natLifecycleActions,
				// The forwarding check and repair (Issue #87).
				remoteScriptActions,
			),
			Note: "冪等。既存 NAT Gateway の削除と EIP 解放を含む。最後に NAT インスタンスの転送を " +
				"Run Command で確認し、壊れていれば直す (停止中なら確認のために起動して戻す)",
			Simulate: true,
		},
		{
			Feature:  "acl",
			Commands: "nakama aws acl add / list / delete",
			Actions: []string{
				"ssm:DeleteParameter",
				"ssm:GetParametersByPath",
				"ssm:PutParameter",
				"wafv2:GetIPSet",
				"wafv2:UpdateIPSet",
			},
			Simulate: true,
		},
		{
			Feature:  "create",
			Commands: "nakama aws create",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				remoteScriptActions, // --setup / --dotfiles / --preset
				[]string{
					"cloudfront:CreateDistribution",
					"cloudfront:CreateVpcOrigin",
					"cloudfront:GetDistribution",
					"cloudfront:GetVpcOrigin",
					"cloudfront:TagResource",
					"ec2:CreateNetworkInterface",
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeManagedPrefixLists",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DescribeSubnets",
					"ssm:PutParameter",
				},
			),
			Note:     "--bare は cloudfront:* を使わない。NAT インスタンスの初回作成を含む",
			Simulate: true,
		},
		{
			Feature:  "create --with-admin / --with-role",
			Commands: "nakama aws create --with-admin / --with-role",
			Actions: []string{
				"iam:AddRoleToInstanceProfile",
				"iam:AttachRolePolicy",
				"iam:CreateInstanceProfile",
				"iam:CreateRole",
				"iam:GetInstanceProfile",
				"iam:GetRole",
				"iam:ListAttachedRolePolicies",
				"iam:ListInstanceProfilesForRole",
				"iam:SimulatePrincipalPolicy",
			},
			Note:     "create 本体の権限に追加で必要 (ADR 0010 の事前検証を含む)",
			Simulate: true,
		},
		{
			Feature:  "list / info",
			Commands: "nakama aws list / info",
			Actions:  mergeActions(resolveInstanceActions),
			Simulate: true,
		},
		{
			Feature:  "open",
			Commands: "nakama aws open",
			Actions: mergeActions(
				resolveInstanceActions,
				[]string{
					"ssm:GetParametersByPath",
					"wafv2:GetIPSet",
				},
			),
			Note: "wafv2:GetIPSet と ssm:GetParametersByPath は許可リスト未登録の " +
				"警告表示のためだけに使い、読めない場合は警告を出さずに続行する",
			Simulate: true,
		},
		{
			Feature:  "kirocrew",
			Commands: "nakama aws kirocrew",
			Actions: mergeActions(
				resolveInstanceActions,
				remoteScriptActions, // the dashboard token is minted on the instance
				[]string{"cloudfront:GetDistribution"},
			),
			Note: "トークンは RSA-OAEP で暗号化して Run Command で受け取る (ADR 0020)。" +
				"code-server のパスワード読み取りは open と同じ",
			Simulate: true,
		},
		{
			Feature:  "kirocrew enable / disable",
			Commands: "nakama aws kirocrew enable / disable",
			Actions: mergeActions(
				resolveInstanceActions,
				remoteScriptActions, // nginx + code-server proxy-domain on the instance
				[]string{
					"cloudfront:CreateDistribution",
					"cloudfront:CreateFunction",
					"cloudfront:DeleteDistribution",
					"cloudfront:DeleteFunction",
					"cloudfront:DescribeFunction",
					"cloudfront:GetDistribution",
					"cloudfront:GetDistributionConfig",
					"cloudfront:PublishFunction",
					"cloudfront:TagResource",
					"cloudfront:UpdateDistribution",
					"cloudfront:UpdateFunction",
					"ssm:DeleteParameters",
					"ssm:PutParameter",
				},
			),
			Note:     "Kiro Crew 専用の distribution と CloudFront Function を作成・削除する",
			Simulate: true,
		},
		{
			Feature:  "connect",
			Commands: "nakama aws connect",
			Actions:  mergeActions(resolveInstanceActions, sessionActions),
			Simulate: true,
		},
		{
			Feature:  "ssh",
			Commands: "nakama aws ssh setup / proxy",
			Actions: mergeActions(
				resolveInstanceActions, sessionActions, remoteScriptActions),
			Note:     "setup の公開鍵投入に SendCommand を使う",
			Simulate: true,
		},
		{
			Feature:  "tunnel",
			Commands: "nakama aws tunnel",
			Actions: mergeActions(
				resolveInstanceActions, sessionActions, remoteScriptActions),
			Note:     "suggest の listen ポート調査に SendCommand を使う。status は資格情報不要",
			Simulate: true,
		},
		{
			Feature:  "start / stop",
			Commands: "nakama aws start / stop",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				[]string{"ec2:StartInstances", "ec2:StopInstances"},
			),
			Note:     "NAT インスタンスの電源連動 (初回は作成) を含む",
			Simulate: true,
		},
		{
			Feature:  "edit",
			Commands: "nakama aws edit",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				remoteScriptActions, // filesystem grow after EBS resize
				[]string{
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeVolumes",
					"ec2:ModifyInstanceAttribute",
					"ec2:ModifyVolume",
					"ec2:StartInstances",
					"ec2:StopInstances",
				},
			),
			Simulate: true,
		},
		{
			Feature:  "update / setup rerun",
			Commands: "nakama aws update / setup rerun",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				remoteScriptActions,
				backupActions, // automatic pre-update backup
				[]string{
					"ec2:RebootInstances",
					"ec2:StartInstances",
					"ec2:StopInstances",
				},
			),
			Note:     "update は更新前の自動バックアップを取得する",
			Simulate: true,
		},
		{
			Feature:  "backup",
			Commands: "nakama aws backup create / list / note / delete",
			Actions:  mergeActions(resolveInstanceActions, backupActions),
			Simulate: true,
		},
		{
			Feature:  "restore",
			Commands: "nakama aws restore",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				[]string{
					"ec2:AttachVolume",
					"ec2:CreateTags",
					"ec2:CreateVolume",
					"ec2:DeleteVolume",
					"ec2:DescribeSnapshots",
					"ec2:DescribeVolumes",
					"ec2:DetachVolume",
					"ec2:ModifyInstanceAttribute",
					"ec2:StopInstances",
				},
			),
			Simulate: true,
		},
		{
			Feature:  "delete",
			Commands: "nakama aws delete",
			Actions: mergeActions(
				resolveInstanceActions,
				natLifecycleActions,
				[]string{
					"cloudfront:DeleteDistribution",
					"cloudfront:DeleteFunction",
					"cloudfront:DeleteVpcOrigin",
					"cloudfront:DescribeFunction",
					"cloudfront:GetDistribution",
					"cloudfront:GetDistributionConfig",
					"cloudfront:GetVpcOrigin",
					"cloudfront:UpdateDistribution",
					"ec2:DeleteNetworkInterface",
					"ec2:DeleteSecurityGroup",
					"ec2:DeleteVolume",
					"ec2:DescribeVolumes",
					"ec2:TerminateInstances",
					"ssm:DeleteParameters",
					"ssm:GetParametersByPath",
				},
			),
			Note: "--bare で作成したインスタンスは cloudfront:* を使わない。" +
				"cloudfront:*Function は kirocrew enable 済みのインスタンスでのみ使う",
			Simulate: true,
		},
		{
			Feature:  "doctor",
			Commands: "nakama aws doctor",
			Actions: []string{
				"iam:SimulatePrincipalPolicy",
				"ssm:GetParametersByPath",
				"wafv2:GetIPSet",
			},
			Note: "読み取り専用。iam:SimulatePrincipalPolicy が無い場合も " +
				"capability 判定が warn になるだけで他の診断は動く",
			Simulate: true,
		},
	}
	for i := range rows {
		rows[i].Actions = mergeActions(rows[i].Actions)
	}
	return rows
}

// mergeActions unions the given action slices, sorted and de-duplicated.
func mergeActions(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range sets {
		for _, a := range set {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	sort.Strings(out)
	return out
}

// simulatedActionUnion returns the union of every Simulate row, the
// action list doctor feeds to SimulatePrincipalPolicy.
func simulatedActionUnion() []string {
	var sets [][]string
	for _, row := range featurePermissionMatrix() {
		if row.Simulate {
			sets = append(sets, row.Actions)
		}
	}
	return mergeActions(sets...)
}

// Markers delimit the generated permissions block in
// docs/requirements.md. The content between them is produced by
// renderPermissionsMarkdown and verified by TestPermissionsDocsSection.
const (
	permissionsDocBegin = "<!-- BEGIN GENERATED: permissions-matrix -->"
	permissionsDocEnd   = "<!-- END GENERATED: permissions-matrix -->"
)

// renderPermissionsMarkdown renders the docs block between the markers
// (markers included).
func renderPermissionsMarkdown() string {
	var b strings.Builder
	b.WriteString(permissionsDocBegin + "\n")
	b.WriteString("<!-- このブロックは internal/service/permissions.go から生成されます。直接編集せず、\n")
	b.WriteString("     NAKAMA_UPDATE_PERMISSIONS_DOCS=1 go test ./internal/service/ -run TestPermissionsDocsSection\n")
	b.WriteString("     で再生成してください。 -->\n")
	b.WriteString("| 機能 | コマンド | 必要な IAM アクション | 備考 |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, row := range featurePermissionMatrix() {
		var codes []string
		for _, a := range row.Actions {
			codes = append(codes, "`"+a+"`")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			row.Feature, row.Commands, strings.Join(codes, ", "), row.Note)
	}
	b.WriteString(permissionsDocEnd)
	return b.String()
}
