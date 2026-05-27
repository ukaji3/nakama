# ADR 0003: HTTP-only CloudFront-to-Origin Connection

- Status: Accepted
- Date: 2026-05-28
- Context: nakama v1 design

## Context

The CloudFront VPC Origins feature lets CloudFront connect to a
private-subnet EC2 instance through a service-managed ENI. The
viewer side (Browser → CloudFront) terminates HTTPS at CloudFront
using its default certificate (`*.cloudfront.net`).

For the origin side (CloudFront → EC2) we have two practical choices:

1. **HTTP** to the origin on port 8080 (or 80).
2. **HTTPS** to the origin on port 443 (or 8443) with a server
   certificate installed on the EC2.

Option 2 requires the EC2 to present a certificate that CloudFront
trusts (a public CA). Public CAs do not issue certificates for
`*.compute.internal` private DNS names, and CloudFront does not
permit self-signed certificates for custom-origin TLS. To use HTTPS
to a private-subnet EC2 we would have to:

- terminate HTTPS at an internal ALB and use a fronting domain that
  has an ACM certificate; or
- run a private CA (ACM Private CA) and import its trust chain into
  CloudFront — but this is not a supported configuration for
  CloudFront origin TLS verification.

## Decision

CloudFront connects to nakama EC2 origins over **plain HTTP on TCP
port 8080**. The viewer-side connection remains HTTPS / WSS.

`code-server` is configured with `cert: false` and binds on
`0.0.0.0:8080`. Inbound traffic on the EC2 SG is restricted to the
CloudFront managed prefix list
`com.amazonaws.global.cloudfront.origin-facing`.

## Rationale

- The user explicitly stated: "ブラウザと CloudFront 間は暗号化されている
  ことが前提" (decision D4). Origin-side HTTP is acceptable.
- The CloudFront ⇄ EC2 path traverses a service-managed ENI and a
  private subnet only — never the public internet.
- This eliminates the operational burden of provisioning, rotating,
  and reissuing TLS certificates on every nakama EC2 instance.

## Consequences

- An attacker who somehow breaches AWS's VPC isolation (out of
  scope for our threat model) could observe HTTP plaintext between
  CloudFront and EC2. We accept this risk because the threat model
  treats AWS-internal network paths as trusted.
- The WAFv2 web ACL (ADR 0002) and the SG inbound restriction to
  CloudFront prefix list together prevent direct HTTP access from
  any other source.
- CloudFront's `Origin Protocol Policy` is set to `http-only`.

## Alternatives Considered

- **HTTPS with self-signed certificate**: not supported by
  CloudFront origin TLS verification.
- **Internal ALB + ACM certificate fronting an internal domain**:
  adds an ALB and an ACM certificate provisioning workflow to
  every nakama instance. Disproportionate for a per-developer tool.
- **HTTPS via ACM Private CA**: not supported as a trust anchor for
  CloudFront origin TLS.

## References

- <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html>
- <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/using-https-cloudfront-to-custom-origin.html>
