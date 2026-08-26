module github.com/ukaji3/nakama

go 1.25.0

require (
	github.com/aws/aws-sdk-go-v2 v1.32.7
	github.com/aws/aws-sdk-go-v2/config v1.28.7
	github.com/aws/aws-sdk-go-v2/service/cloudfront v1.42.0
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.197.0
	github.com/aws/aws-sdk-go-v2/service/iam v1.38.0
	github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi v1.24.4
	github.com/aws/aws-sdk-go-v2/service/ssm v1.56.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.33.3
	github.com/aws/aws-sdk-go-v2/service/wafv2 v1.55.4
	github.com/aws/session-manager-plugin v0.0.0-00010101000000-000000000000
	github.com/aws/smithy-go v1.22.1
	github.com/spf13/cobra v1.8.1
	github.com/twinj/uuid v1.0.0
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/aws/aws-sdk-go-v2/credentials v1.17.48 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.16.22 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.3.26 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.6.26 // indirect
	github.com/aws/aws-sdk-go-v2/internal/ini v1.8.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.12.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.12.7 // indirect
	github.com/aws/aws-sdk-go-v2/service/kms v1.37.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.24.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.28.7 // indirect
	github.com/cihub/seelog v0.0.0-20170130134532-f561c5e57575 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/xtaci/smux v1.5.34 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/term v0.45.0 // indirect
)

replace github.com/aws/session-manager-plugin => ./third_party/session-manager-plugin

replace github.com/twinj/uuid => ./third_party/twinj-uuid
