# ADR 0002: WAFv2 IP Set for Source-IP Access Control

- Status: Accepted
- Date: 2026-05-28
- Context: nakama v1 design

## Context

The original task description used the phrase "CloudFront has a
Security Group" and proposed managing source-IP access with
`nakama aws acl`. CloudFront distributions, however, do not have
Security Groups. Security Groups in the VPC Origins design control
**Origin-side** ingress (CloudFront ENI → EC2), not viewer-side
ingress (Browser → CloudFront).

To gate access from end-user IP addresses, CloudFront accepts an
associated **AWS WAFv2 web ACL** (CLOUDFRONT scope), which is the
standard primitive for IP-based allowlisting at the CDN layer.

## Decision

Use a single shared **WAFv2 web ACL** (`nakama-waf`) with an
associated **IP set** (`nakama-allowed-ips`) to manage source-IP
allowlisting for every nakama CloudFront distribution.

`nakama aws acl {add,list,delete}` operates on the IP set.
`add` without arguments queries `https://checkip.amazonaws.com/` to
auto-detect the caller's public IPv4 address.

The web ACL's default action is **block**; a single rule allows
traffic whose source IP is in the IP set.

## Rationale

- WAFv2 IP sets accept `Address` entries in IPv4 / IPv6 CIDR form
  and atomic updates via `LockToken`-protected `UpdateIPSet`.
- Cost (~$5 / web ACL / month + $1 / rule / month) is acceptable
  for an individual-developer tool and avoids the overhead of
  Lambda@Edge / CloudFront Functions.
- WAF integrates natively with CloudFront via the `WebACLId`
  field on the distribution config; no additional plumbing.

## Consequences

- One shared web ACL is associated with every nakama distribution;
  there is no per-instance allowlist (out of v1 scope).
- IPv6 CIDRs are not supported by `nakama aws acl` in v1, even though
  WAFv2 itself supports them. Documented as a limitation in
  `requirements.md` §7.
- The web ACL must be created in `us-east-1` (CLOUDFRONT scope
  requirement). `internal/awsx/waf.go` forces region `us-east-1`
  in the WAFv2 client constructor.

## Alternatives Considered

- **CloudFront Functions** with a hard-coded IP allowlist: rejected
  because every IP change rewrites and republishes the function code
  (slow, bad UX) and the function size is bounded.
- **Lambda@Edge**: rejected as overkill and incompatible with VPC
  Origins for origin-request triggers.
- **VPC Origin SG-only restriction**: invalid — the VPC Origin SG
  controls CloudFront → EC2 ingress, not Browser → CloudFront ingress.

## References

- <https://docs.aws.amazon.com/waf/latest/developerguide/cloudfront-features.html>
- <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/distribution-web-awswaf.html>

## 追記 (2026-10-23, Issue #104)

引数なしの `acl add` は、`https://checkip.amazonaws.com/` と `https://ifconfig.io/ip` に 3 回ずつ問い合わせ、見つかった公開の IPv4 アドレスをすべて登録するようになった (出口を冗長化したネットワークのため)。ADR 0025 を参照。
