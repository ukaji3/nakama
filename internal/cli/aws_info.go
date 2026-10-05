package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

type infoFlags struct {
	showPassword bool
}

func newAWSInfoCommand() *cobra.Command {
	f := &infoFlags{}
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show connection details for a nakama instance",
		Long: `Prints the CloudFront URL, instance state and metadata for the
named instance. The code-server password stored in SSM Parameter Store
is NOT printed unless ` + "`--show-password`" + ` is supplied.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			info, err := service.NewService(clients).GetInstanceInfo(ctx, args[0], f.showPassword)
			if err != nil {
				return err
			}
			return renderOutput(cmd.OutOrStdout(), global.Output, info, formatInfo(info, f.showPassword))
		},
	}
	cmd.Flags().BoolVar(&f.showPassword, "show-password", false, "Print the code-server password in plain text")
	return cmd
}

func formatInfo(i *service.InstanceInfo, showPassword bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name              : %s\n", i.Name)
	fmt.Fprintf(&b, "instance_id       : %s\n", i.InstanceID)
	fmt.Fprintf(&b, "state             : %s\n", i.State)
	fmt.Fprintf(&b, "instance_type     : %s\n", i.InstanceType)
	fmt.Fprintf(&b, "eni_id            : %s\n", i.ENIID)
	fmt.Fprintf(&b, "security_group_id : %s\n", i.SecurityGroupID)
	fmt.Fprintf(&b, "vpc_origin_id     : %s\n", i.VPCOriginID)
	fmt.Fprintf(&b, "distribution_id   : %s\n", i.DistributionID)
	fmt.Fprintf(&b, "url               : %s\n", i.URL)
	fmt.Fprintf(&b, "code_server_ver   : %s\n", i.CodeServerVersion)
	if i.KiroCrewURL != "" {
		fmt.Fprintf(&b, "kirocrew_url      : %s\n", i.KiroCrewURL)
	}
	if showPassword {
		fmt.Fprintf(&b, "password          : %s", i.Password)
	} else {
		fmt.Fprintf(&b, "password          : (use --show-password to reveal)")
	}
	return b.String()
}
