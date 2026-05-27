# ADR 0001: CloudFront VPC Origins for Private EC2 Code-Server

- Status: Accepted
- Date: 2026-05-28
- Context: nakama v1 design

## Context

nakama needs to expose a code-server EC2 instance to a developer's
browser over WSS while keeping the EC2 instance unreachable from the
public internet. The instance lives in a private subnet without a
public IP and only egresses through a NAT Gateway.

Available options for fronting the private instance with HTTPS / WSS:

1. **Public ALB + Internet-facing**: ALB in public subnet, EC2 in private subnet.
   ALB needs an ACM certificate; SG rules at ALB control access.
2. **API Gateway HTTP API + VPC Link**: terminates at API Gateway.
   WSS support exists for WebSocket APIs but the configuration is
   non-trivial and pricing differs.
3. **CloudFront with public custom origin**: requires a public IP /
   public ALB; loses the "private origin" property.
4. **CloudFront VPC Origins (private origin)**: CloudFront connects
   to a private-subnet EC2 / ALB / NLB through a service-managed ENI,
   leaving the origin without any public IP.

## Decision

Adopt **CloudFront VPC Origins** (option 4) as the front-door for
nakama instances.

## Rationale

- The user explicitly asked for a "Private Origin" architecture.
- CloudFront VPC Origins is GA in `ap-northeast-1` (with the
  exception of AZ `apne1-az3`). Source:
  <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html>
- WebSocket support over VPC Origins reached GA on 2026-05-01.
  Source: <https://aws.amazon.com/about-aws/whats-new/2026/05/amazon-cloudfront-websockets-vpc-origins/>
  > "Amazon CloudFront now supports WebSockets traffic through Virtual
  > Private Cloud (VPC) origins, enabling you to use CloudFront as the
  > single entry point for real-time applications hosted entirely in
  > private subnets."
- The origin EC2 needs no ACM certificate (CloudFront → Origin uses
  HTTP:8080 inside the VPC; ADR 0003 covers this).
- WAFv2 IP set + web ACL gives us per-source-IP control without
  needing an ALB or self-managed firewall.

## Consequences

- nakama is locked to AWS regions where CloudFront VPC Origins is
  supported. AZ `apne1-az3` must be excluded from subnet placement.
- gRPC and Lambda@Edge origin request/response triggers are not
  supported on VPC Origins. nakama does not need them.
- WebSocket is HTTP/1.1 only when going through CloudFront. code-server
  is compatible.
- VPC Origin / Distribution propagation latency (~15 min) dominates
  `create` / `delete` runtime.

## Alternatives Considered

- ALB-based fronting was rejected because it leaves the origin with
  a publicly resolvable, internet-routable hostname (the ALB itself),
  contradicting the "Private Origin" requirement.
- API Gateway WebSocket APIs would change the protocol semantics
  exposed to code-server (proxy WebSocket vs. native upgrade) and
  add per-message billing.

## References

- <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-vpc-origins.html>
- <https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/distribution-working-with.websockets.html>
