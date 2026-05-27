package cli

import (
	"github.com/spf13/cobra"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/service"
)

func newAWSStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start the EC2 instance backing a nakama instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			id, err := service.NewService(clients).StartInstance(ctx, args[0])
			if err != nil {
				return err
			}
			res := map[string]string{"name": args[0], "instance_id": id, "action": "start"}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "starting "+args[0]+" ("+id+")")
		},
	}
}

func newAWSStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the EC2 instance backing a nakama instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := withCancel(cmd)
			clients, err := awsx.NewClients(ctx)
			if err != nil {
				return err
			}
			id, err := service.NewService(clients).StopInstance(ctx, args[0])
			if err != nil {
				return err
			}
			res := map[string]string{"name": args[0], "instance_id": id, "action": "stop"}
			return renderOutput(cmd.OutOrStdout(), global.Output, res, "stopping "+args[0]+" ("+id+")")
		},
	}
}
