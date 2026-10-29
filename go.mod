module github.com/ukaji3/nakama

go 1.26.0

require (
	github.com/aws/aws-sdk-go-v2 v1.47.4
	github.com/aws/aws-sdk-go-v2/config v1.33.10
	github.com/aws/aws-sdk-go-v2/service/cloudfront v1.75.1
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.341.0
	github.com/aws/aws-sdk-go-v2/service/iam v1.64.5
	github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi v1.41.6
	github.com/aws/aws-sdk-go-v2/service/ssm v1.81.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.5
	github.com/aws/aws-sdk-go-v2/service/wafv2 v1.83.6
	github.com/aws/session-manager-plugin v0.0.0-00010101000000-000000000000
	github.com/aws/smithy-go v1.28.6
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-ieproxy v0.0.12
	github.com/spf13/cobra v1.10.2
	github.com/twinj/uuid v1.0.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/aws/aws-sdk-go-v2/credentials v1.20.10 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.7 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.7 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.22 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/kms v1.37.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.10.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.38.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.43.5 // indirect
	github.com/cihub/seelog v0.0.0-20170130134532-f561c5e57575 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/xtaci/smux v1.5.34 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/aws/session-manager-plugin => ./third_party/session-manager-plugin

replace github.com/twinj/uuid => ./third_party/twinj-uuid
