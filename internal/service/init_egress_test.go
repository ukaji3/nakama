package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/ukaji3/nakama/internal/awsx"
	"github.com/ukaji3/nakama/internal/config"
	"github.com/ukaji3/nakama/internal/naming"
	"github.com/ukaji3/nakama/internal/tagging"
)

// fakeInfraEC2 is a small stateful model of the shared infrastructure
// the egress code touches (ADR 0026): the NAT instance and its security
// group, the route tables, the NAT gateways and their Elastic IPs. It
// records the mutating calls in order, so the tests can check that
// egress moves before the old NAT goes away.
type fakeInfraEC2 struct {
	awsx.EC2API

	mu    sync.Mutex
	calls []string
	seq   int

	vpcExists         bool
	activeCodeServers []ec2types.Instance

	natInstances []*ec2types.Instance
	natSGID      string
	routeTables  []*ec2types.RouteTable
	natGateways  []*ec2types.NatGateway
	eips         []*ec2types.Address

	createNatGatewayErr error
	// newNatGatewayState is the state a created NAT gateway reaches
	// (available unless set).
	newNatGatewayState ec2types.NatGatewayState
}

func (f *fakeInfraEC2) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeInfraEC2) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

// callIndex returns the position of the first recorded call with the
// given prefix, or -1.
func (f *fakeInfraEC2) callIndex(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func (f *fakeInfraEC2) has(prefix string) bool { return f.callIndex(prefix) >= 0 }

func filterValues(filters []ec2types.Filter, name string) []string {
	for _, fl := range filters {
		if aws.ToString(fl.Name) == name {
			return fl.Values
		}
	}
	return nil
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

func tagsOf(specs []ec2types.TagSpecification) []ec2types.Tag {
	var tags []ec2types.Tag
	for _, s := range specs {
		tags = append(tags, s.Tags...)
	}
	return tags
}

func (f *fakeInfraEC2) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if !f.vpcExists {
		return &ec2.DescribeVpcsOutput{}, nil
	}
	return &ec2.DescribeVpcsOutput{Vpcs: []ec2types.Vpc{{VpcId: aws.String("vpc-1")}}}, nil
}

func (f *fakeInfraEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.natSGID == "" {
		return &ec2.DescribeSecurityGroupsOutput{}, nil
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []ec2types.SecurityGroup{{GroupId: aws.String(f.natSGID)}}}, nil
}

func (f *fakeInfraEC2) CreateSecurityGroup(context.Context, *ec2.CreateSecurityGroupInput, ...func(*ec2.Options)) (*ec2.CreateSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.natSGID = f.nextID("sg-nat")
	f.record("CreateSecurityGroup:%s", f.natSGID)
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(f.natSGID)}, nil
}

func (f *fakeInfraEC2) AuthorizeSecurityGroupIngress(context.Context, *ec2.AuthorizeSecurityGroupIngressInput, ...func(*ec2.Options)) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func (f *fakeInfraEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ ...func(*ec2.Options)) (*ec2.DeleteSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.natInstances {
		if i.State.Name != ec2types.InstanceStateNameTerminated {
			return nil, &smithy.GenericAPIError{Code: "DependencyViolation"}
		}
	}
	if aws.ToString(in.GroupId) != f.natSGID {
		return nil, &smithy.GenericAPIError{Code: "InvalidGroup.NotFound"}
	}
	f.record("DeleteSecurityGroup:%s", f.natSGID)
	f.natSGID = ""
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *fakeInfraEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(in.InstanceIds) > 0 {
		for _, i := range f.natInstances {
			if aws.ToString(i.InstanceId) == in.InstanceIds[0] {
				return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{*i}}}}, nil
			}
		}
		return nil, &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}
	}
	states := filterValues(in.Filters, "instance-state-name")
	var out []ec2types.Instance
	switch comp := filterValues(in.Filters, "tag:"+tagging.KeyComponent); {
	case slices.Contains(comp, tagging.ComponentNATInstance):
		for _, i := range f.natInstances {
			if len(states) == 0 || slices.Contains(states, string(i.State.Name)) {
				out = append(out, *i)
			}
		}
	case slices.Contains(comp, tagging.ComponentEC2):
		out = f.activeCodeServers
	}
	if len(out) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeInfraEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID("i-nat")
	inst := &ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: in.InstanceType,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Tags:         tagsOf(in.TagSpecifications),
		NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
			NetworkInterfaceId: aws.String("eni-" + id),
			Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
		}},
	}
	f.natInstances = append(f.natInstances, inst)
	f.record("RunInstances:%s", id)
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{*inst}}, nil
}

func (f *fakeInfraEC2) ModifyInstanceAttribute(context.Context, *ec2.ModifyInstanceAttributeInput, ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error) {
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

func (f *fakeInfraEC2) setInstanceState(id string, st ec2types.InstanceStateName) {
	for _, i := range f.natInstances {
		if aws.ToString(i.InstanceId) == id {
			i.State = &ec2types.InstanceState{Name: st}
		}
	}
}

func (f *fakeInfraEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		f.setInstanceState(id, ec2types.InstanceStateNameTerminated)
		f.record("TerminateInstances:%s", id)
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeInfraEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		f.setInstanceState(id, ec2types.InstanceStateNameRunning)
		f.record("StartInstances:%s", id)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeInfraEC2) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range in.InstanceIds {
		f.setInstanceState(id, ec2types.InstanceStateNameStopped)
		f.record("StopInstances:%s", id)
	}
	return &ec2.StopInstancesOutput{}, nil
}

func (f *fakeInfraEC2) routeTableByID(id string) *ec2types.RouteTable {
	for _, rt := range f.routeTables {
		if aws.ToString(rt.RouteTableId) == id {
			return rt
		}
	}
	return nil
}

func (f *fakeInfraEC2) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(in.RouteTableIds) > 0 {
		if rt := f.routeTableByID(in.RouteTableIds[0]); rt != nil {
			return &ec2.DescribeRouteTablesOutput{RouteTables: []ec2types.RouteTable{*rt}}, nil
		}
		return &ec2.DescribeRouteTablesOutput{}, nil
	}
	names := filterValues(in.Filters, "tag:"+tagging.NameTag)
	for _, rt := range f.routeTables {
		if slices.Contains(names, tagValue(rt.Tags, tagging.NameTag)) {
			return &ec2.DescribeRouteTablesOutput{RouteTables: []ec2types.RouteTable{*rt}}, nil
		}
	}
	return &ec2.DescribeRouteTablesOutput{}, nil
}

func (f *fakeInfraEC2) CreateRouteTable(_ context.Context, in *ec2.CreateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := &ec2types.RouteTable{RouteTableId: aws.String(f.nextID("rtb")), Tags: tagsOf(in.TagSpecifications)}
	f.routeTables = append(f.routeTables, rt)
	return &ec2.CreateRouteTableOutput{RouteTable: rt}, nil
}

func routeTarget(r ec2types.Route) string {
	return firstNonEmpty(aws.ToString(r.NatGatewayId), aws.ToString(r.NetworkInterfaceId), aws.ToString(r.GatewayId))
}

func (f *fakeInfraEC2) CreateRoute(_ context.Context, in *ec2.CreateRouteInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := f.routeTableByID(aws.ToString(in.RouteTableId))
	r := ec2types.Route{DestinationCidrBlock: in.DestinationCidrBlock, GatewayId: in.GatewayId,
		NetworkInterfaceId: in.NetworkInterfaceId, NatGatewayId: in.NatGatewayId}
	rt.Routes = append(rt.Routes, r)
	f.record("CreateRoute:%s:%s", tagValue(rt.Tags, tagging.NameTag), routeTarget(r))
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeInfraEC2) ReplaceRoute(_ context.Context, in *ec2.ReplaceRouteInput, _ ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := f.routeTableByID(aws.ToString(in.RouteTableId))
	r := ec2types.Route{DestinationCidrBlock: in.DestinationCidrBlock, GatewayId: in.GatewayId,
		NetworkInterfaceId: in.NetworkInterfaceId, NatGatewayId: in.NatGatewayId}
	for i := range rt.Routes {
		if aws.ToString(rt.Routes[i].DestinationCidrBlock) == "0.0.0.0/0" {
			rt.Routes[i] = r
		}
	}
	f.record("ReplaceRoute:%s:%s", tagValue(rt.Tags, tagging.NameTag), routeTarget(r))
	return &ec2.ReplaceRouteOutput{}, nil
}

func (f *fakeInfraEC2) AssociateRouteTable(_ context.Context, in *ec2.AssociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := f.routeTableByID(aws.ToString(in.RouteTableId))
	rt.Associations = append(rt.Associations, ec2types.RouteTableAssociation{SubnetId: in.SubnetId})
	return &ec2.AssociateRouteTableOutput{}, nil
}

func (f *fakeInfraEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ec2types.NatGateway
	for _, gw := range f.natGateways {
		if len(in.NatGatewayIds) > 0 && !slices.Contains(in.NatGatewayIds, aws.ToString(gw.NatGatewayId)) {
			continue
		}
		out = append(out, *gw)
	}
	return &ec2.DescribeNatGatewaysOutput{NatGateways: out}, nil
}

func (f *fakeInfraEC2) eipByAllocation(id string) *ec2types.Address {
	for _, a := range f.eips {
		if aws.ToString(a.AllocationId) == id {
			return a
		}
	}
	return nil
}

func (f *fakeInfraEC2) CreateNatGateway(_ context.Context, in *ec2.CreateNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.CreateNatGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createNatGatewayErr != nil {
		return nil, f.createNatGatewayErr
	}
	if in.ConnectivityType != ec2types.ConnectivityTypePublic || aws.ToString(in.SubnetId) == "" {
		return nil, fmt.Errorf("want a public NAT gateway in a subnet, got %+v", in)
	}
	eip := f.eipByAllocation(aws.ToString(in.AllocationId))
	if eip == nil || eip.AssociationId != nil {
		return nil, &smithy.GenericAPIError{Code: "InvalidAllocationID.NotFound"}
	}
	id := f.nextID("nat")
	eip.AssociationId = aws.String("eipassoc-" + id)
	state := f.newNatGatewayState
	if state == "" {
		state = ec2types.NatGatewayStateAvailable
	}
	gw := &ec2types.NatGateway{
		NatGatewayId: aws.String(id),
		State:        state,
		VpcId:        aws.String("vpc-1"),
		SubnetId:     in.SubnetId,
		Tags:         tagsOf(in.TagSpecifications),
		NatGatewayAddresses: []ec2types.NatGatewayAddress{{
			AllocationId: eip.AllocationId, PublicIp: eip.PublicIp, IsPrimary: aws.Bool(true),
		}},
	}
	if state == ec2types.NatGatewayStateFailed {
		gw.FailureMessage = aws.String("Insufficient capacity")
	}
	f.natGateways = append(f.natGateways, gw)
	f.record("CreateNatGateway:%s:%s", id, aws.ToString(in.AllocationId))
	return &ec2.CreateNatGatewayOutput{NatGateway: &ec2types.NatGateway{NatGatewayId: aws.String(id), State: ec2types.NatGatewayStatePending}}, nil
}

func (f *fakeInfraEC2) DeleteNatGateway(_ context.Context, in *ec2.DeleteNatGatewayInput, _ ...func(*ec2.Options)) (*ec2.DeleteNatGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, gw := range f.natGateways {
		if aws.ToString(gw.NatGatewayId) != aws.ToString(in.NatGatewayId) {
			continue
		}
		gw.State = ec2types.NatGatewayStateDeleted
		for _, a := range gw.NatGatewayAddresses {
			if eip := f.eipByAllocation(aws.ToString(a.AllocationId)); eip != nil {
				eip.AssociationId = nil
			}
		}
		f.record("DeleteNatGateway:%s", aws.ToString(in.NatGatewayId))
		return &ec2.DeleteNatGatewayOutput{}, nil
	}
	return nil, &smithy.GenericAPIError{Code: "NatGatewayNotFound"}
}

func (f *fakeInfraEC2) DescribeAddresses(context.Context, *ec2.DescribeAddressesInput, ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ec2types.Address
	for _, a := range f.eips {
		out = append(out, *a)
	}
	return &ec2.DescribeAddressesOutput{Addresses: out}, nil
}

func (f *fakeInfraEC2) AllocateAddress(_ context.Context, in *ec2.AllocateAddressInput, _ ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tagValue(tagsOf(in.TagSpecifications), tagging.NameTag) != naming.NATGWEIP() {
		return nil, errors.New("the Elastic IP must carry the NAT gateway EIP name")
	}
	id := f.nextID("eipalloc")
	a := &ec2types.Address{AllocationId: aws.String(id), PublicIp: aws.String(fmt.Sprintf("203.0.113.%d", f.seq))}
	f.eips = append(f.eips, a)
	f.record("AllocateAddress:%s", id)
	return &ec2.AllocateAddressOutput{AllocationId: a.AllocationId, PublicIp: a.PublicIp}, nil
}

func (f *fakeInfraEC2) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.eips {
		if aws.ToString(a.AllocationId) != aws.ToString(in.AllocationId) {
			continue
		}
		if a.AssociationId != nil {
			return nil, &smithy.GenericAPIError{Code: "InvalidIPAddress.InUse"}
		}
		f.eips = append(f.eips[:i], f.eips[i+1:]...)
		f.record("ReleaseAddress:%s", aws.ToString(in.AllocationId))
		return &ec2.ReleaseAddressOutput{}, nil
	}
	return nil, &smithy.GenericAPIError{Code: "InvalidAllocationID.NotFound"}
}

// fakeInfraSSM adds Parameter Store to fakeNATSSM (the NAT instance's
// Run Command): the AMI parameter, the recorded NAT mode, and a log of
// the forwarding checks into the EC2 fake's call list.
type fakeInfraSSM struct {
	*fakeNATSSM
	ec2    *fakeInfraEC2
	params map[string]string
}

func (f *fakeInfraSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	name := aws.ToString(in.Name)
	if name == config.NATInstanceAMISSMParameter {
		return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String("ami-al2023")}}, nil
	}
	if v, ok := f.params[name]; ok {
		return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
	}
	return nil, &ssmtypes.ParameterNotFound{}
}

func (f *fakeInfraSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.params[aws.ToString(in.Name)] = aws.ToString(in.Value)
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeInfraSSM) SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.ec2.mu.Lock()
	f.ec2.record("SendCommand:%s", in.InstanceIds[0])
	f.ec2.mu.Unlock()
	return f.fakeNATSSM.SendCommand(ctx, in, opts...)
}

// infraEnv bundles the fakes and an InitResult as init has it when the
// egress step starts (VPC, IGW and subnets known).
type infraEnv struct {
	ec2 *fakeInfraEC2
	ssm *fakeInfraSSM
	svc *Service
	res *InitResult
}

func newInfraEnv(mode string, nat *fakeNATSSM) *infraEnv {
	if nat == nil {
		nat = &fakeNATSSM{healthy: true}
	}
	e := &fakeInfraEC2{vpcExists: true}
	s := &fakeInfraSSM{fakeNATSSM: nat, ec2: e, params: map[string]string{}}
	return &infraEnv{
		ec2: e,
		ssm: s,
		svc: &Service{clients: &awsx.Clients{EC2: e, SSM: s}},
		res: &InitResult{
			VPCID: "vpc-1", InternetGatewayID: "igw-1", PublicSubnetID: "subnet-public",
			PrivateSubnetIDs: map[string]string{"ap-northeast-1a": "subnet-private-1a"},
			NATMode:          mode,
		},
	}
}

// withRouteTables gives the environment existing route tables, the
// private one routing 0.0.0.0/0 to the given route.
func (e *infraEnv) withRouteTables(private ec2types.Route) *infraEnv {
	private.DestinationCidrBlock = aws.String("0.0.0.0/0")
	nameTag := func(n string) []ec2types.Tag {
		return []ec2types.Tag{{Key: aws.String(tagging.NameTag), Value: aws.String(n)}, {Key: aws.String(tagging.KeyManaged), Value: aws.String("true")}}
	}
	e.ec2.routeTables = []*ec2types.RouteTable{
		{
			RouteTableId: aws.String("rtb-public"), Tags: nameTag(naming.PublicRouteTable()),
			Routes:       []ec2types.Route{{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-1")}},
			Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-public")}},
		},
		{
			RouteTableId: aws.String("rtb-private"), Tags: nameTag(naming.PrivateRouteTable()),
			Routes:       []ec2types.Route{private},
			Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-private-1a")}},
		},
	}
	return e
}

// withNATInstance adds a running NAT instance and its security group.
func (e *infraEnv) withNATInstance(id string, state ec2types.InstanceStateName) *infraEnv {
	e.ec2.natInstances = append(e.ec2.natInstances, &ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: ec2types.InstanceType(config.NATInstanceType),
		State:        &ec2types.InstanceState{Name: state},
		NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
			NetworkInterfaceId: aws.String("eni-" + id),
			Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
		}},
	})
	e.ec2.natSGID = "sg-nat"
	return e
}

// withNATGateway adds a NAT gateway with an associated Elastic IP.
func (e *infraEnv) withNATGateway(id string, state ec2types.NatGatewayState) *infraEnv {
	alloc := "eipalloc-" + id
	e.ec2.eips = append(e.ec2.eips, &ec2types.Address{
		AllocationId: aws.String(alloc), PublicIp: aws.String("198.51.100.7"),
		AssociationId: aws.String("eipassoc-" + id),
	})
	gw := &ec2types.NatGateway{
		NatGatewayId: aws.String(id), State: state, VpcId: aws.String("vpc-1"), SubnetId: aws.String("subnet-public"),
		NatGatewayAddresses: []ec2types.NatGatewayAddress{{AllocationId: aws.String(alloc), PublicIp: aws.String("198.51.100.7"), IsPrimary: aws.Bool(true)}},
	}
	if state == ec2types.NatGatewayStateFailed {
		gw.FailureMessage = aws.String("Elastic IP address could not be associated")
		// A failed NAT gateway holds no address.
		e.ec2.eips[len(e.ec2.eips)-1].AssociationId = nil
	}
	e.ec2.natGateways = append(e.ec2.natGateways, gw)
	return e
}

func (e *infraEnv) privateRoute(t *testing.T) string {
	t.Helper()
	for _, rt := range e.ec2.routeTables {
		if tagValue(rt.Tags, tagging.NameTag) != naming.PrivateRouteTable() {
			continue
		}
		for _, r := range rt.Routes {
			if aws.ToString(r.DestinationCidrBlock) == "0.0.0.0/0" {
				return routeTarget(r)
			}
		}
	}
	t.Fatal("the private route table has no default route")
	return ""
}

func (e *infraEnv) liveNATInstances() []string {
	var ids []string
	for _, i := range e.ec2.natInstances {
		if i.State.Name != ec2types.InstanceStateNameTerminated {
			ids = append(ids, aws.ToString(i.InstanceId))
		}
	}
	return ids
}

func (e *infraEnv) liveNATGateways() []string {
	var ids []string
	for _, gw := range e.ec2.natGateways {
		if gw.State != ec2types.NatGatewayStateDeleted {
			ids = append(ids, aws.ToString(gw.NatGatewayId))
		}
	}
	return ids
}

func assertOrder(t *testing.T, e *fakeInfraEC2, prefixes ...string) {
	t.Helper()
	last := -1
	for _, p := range prefixes {
		i := e.callIndex(p)
		if i < 0 {
			t.Fatalf("call %q was not made; calls:\n  %s", p, strings.Join(e.calls, "\n  "))
		}
		if i < last {
			t.Fatalf("call %q came too early; calls:\n  %s", p, strings.Join(e.calls, "\n  "))
		}
		last = i
	}
}

func TestInitEgress_GatewayNewEnvironment(t *testing.T) {
	shortNATGatewayWaits(t)
	env := newInfraEnv(NATModeGateway, nil)

	checked, err := env.svc.initEgress(context.Background(), env.res)
	if err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	if checked {
		t.Error("the gateway mode runs no forwarding check")
	}
	assertOrder(t, env.ec2, "AllocateAddress:", "CreateNatGateway:", "CreateRoute:"+naming.PrivateRouteTable()+":nat-")
	if got := env.privateRoute(t); got != env.res.NATGatewayID || !strings.HasPrefix(got, "nat-") {
		t.Errorf("private route -> %q, want the NAT gateway %q", got, env.res.NATGatewayID)
	}
	if env.res.NATGatewayPublicIP == "" || env.res.NATGatewayEIPAllocationID == "" {
		t.Errorf("result lacks the egress IP / allocation: %+v", env.res)
	}
	if env.ec2.has("RunInstances") || env.ec2.has("CreateSecurityGroup") {
		t.Errorf("the gateway mode must not create a NAT instance; calls: %v", env.ec2.calls)
	}
	if env.res.TerminatedNATInstanceID != "" || env.res.DeletedNATInstanceSGID != "" {
		t.Errorf("nothing to remove in a new environment: %+v", env.res)
	}
}

func TestInitEgress_SwitchInstanceToGateway(t *testing.T) {
	shortNATGatewayWaits(t)
	env := newInfraEnv(NATModeGateway, nil).
		withNATInstance("i-nat", ec2types.InstanceStateNameRunning).
		withRouteTables(ec2types.Route{NetworkInterfaceId: aws.String("eni-i-nat")})

	if _, err := env.svc.initEgress(context.Background(), env.res); err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	// The NAT gateway is available before the route moves, and the NAT
	// instance goes only after the route moved.
	assertOrder(t, env.ec2,
		"CreateNatGateway:",
		"ReplaceRoute:"+naming.PrivateRouteTable()+":nat-",
		"TerminateInstances:i-nat",
		"DeleteSecurityGroup:sg-nat")
	if got := env.liveNATInstances(); len(got) != 0 {
		t.Errorf("NAT instances left: %v", got)
	}
	if env.ec2.natSGID != "" {
		t.Errorf("the NAT instance security group is left: %s", env.ec2.natSGID)
	}
	if env.res.TerminatedNATInstanceID != "i-nat" || env.res.DeletedNATInstanceSGID != "sg-nat" {
		t.Errorf("result = %+v", env.res)
	}
	if env.ec2.has("ReplaceRoute:" + naming.PublicRouteTable()) {
		t.Error("the public route must not change")
	}
}

func TestInitEgress_GatewayCreationFailureKeepsTheNATInstance(t *testing.T) {
	shortNATGatewayWaits(t)
	for _, c := range []struct {
		name  string
		setup func(*fakeInfraEC2)
	}{
		{"CreateNatGateway fails", func(f *fakeInfraEC2) {
			f.createNatGatewayErr = &smithy.GenericAPIError{Code: "NatGatewayLimitExceeded"}
		}},
		{"the NAT gateway fails", func(f *fakeInfraEC2) { f.newNatGatewayState = ec2types.NatGatewayStateFailed }},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newInfraEnv(NATModeGateway, nil).
				withNATInstance("i-nat", ec2types.InstanceStateNameStopped).
				withRouteTables(ec2types.Route{NetworkInterfaceId: aws.String("eni-i-nat")})
			c.setup(env.ec2)

			if _, err := env.svc.initEgress(context.Background(), env.res); err == nil {
				t.Fatal("initEgress succeeded, want an error")
			}
			if got := env.privateRoute(t); got != "eni-i-nat" {
				t.Errorf("private route -> %q; it must stay on the NAT instance", got)
			}
			if env.ec2.has("TerminateInstances") || env.ec2.has("DeleteSecurityGroup") {
				t.Errorf("the NAT instance must stay; calls: %v", env.ec2.calls)
			}
		})
	}
}

func TestInitEgress_GatewayIsIdempotent(t *testing.T) {
	shortNATGatewayWaits(t)
	env := newInfraEnv(NATModeGateway, nil).
		withNATGateway("nat-1", ec2types.NatGatewayStateAvailable).
		withRouteTables(ec2types.Route{NatGatewayId: aws.String("nat-1")})

	if _, err := env.svc.initEgress(context.Background(), env.res); err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	for _, c := range []string{"CreateNatGateway", "AllocateAddress", "ReplaceRoute", "CreateRoute", "ReleaseAddress", "TerminateInstances", "DeleteNatGateway"} {
		if env.ec2.has(c) {
			t.Errorf("a second init changed something (%s); calls: %v", c, env.ec2.calls)
		}
	}
	if env.res.NATGatewayID != "nat-1" || env.res.NATGatewayPublicIP != "198.51.100.7" {
		t.Errorf("result = %+v", env.res)
	}
}

func TestInitEgress_GatewayReplacesAFailedNATGateway(t *testing.T) {
	shortNATGatewayWaits(t)
	env := newInfraEnv(NATModeGateway, nil).
		withNATGateway("nat-old", ec2types.NatGatewayStateFailed).
		withRouteTables(ec2types.Route{NatGatewayId: aws.String("nat-old")})

	if _, err := env.svc.initEgress(context.Background(), env.res); err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	assertOrder(t, env.ec2, "DeleteNatGateway:nat-old", "CreateNatGateway:", "ReplaceRoute:"+naming.PrivateRouteTable()+":nat-")
	// The failed gateway's Elastic IP is free again and is reused.
	if env.ec2.has("AllocateAddress") {
		t.Errorf("the free Elastic IP should be reused; calls: %v", env.ec2.calls)
	}
	if env.res.DeletedNATGatewayID != "nat-old" || env.res.NATGatewayID == "nat-old" {
		t.Errorf("result = %+v", env.res)
	}
}

func TestInitEgress_SwitchGatewayToInstance(t *testing.T) {
	fastNATTimers(t)
	shortNATGatewayWaits(t)
	env := newInfraEnv(NATModeInstance, &fakeNATSSM{healthy: true}).
		withNATGateway("nat-1", ec2types.NatGatewayStateAvailable).
		withRouteTables(ec2types.Route{NatGatewayId: aws.String("nat-1")})

	checked, err := env.svc.initEgress(context.Background(), env.res)
	if err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	if !checked {
		t.Error("the forwarding check must run before the route moves to the NAT instance")
	}
	// The NAT instance is built and its forwarding checked before the
	// route moves; the NAT gateway goes after.
	assertOrder(t, env.ec2,
		"RunInstances:",
		"SendCommand:",
		"ReplaceRoute:"+naming.PrivateRouteTable()+":eni-",
		"DeleteNatGateway:nat-1",
		"ReleaseAddress:eipalloc-nat-1")
	if got := env.liveNATGateways(); len(got) != 0 {
		t.Errorf("NAT gateways left: %v", got)
	}
	if len(env.ec2.eips) != 0 {
		t.Errorf("Elastic IPs left: %+v", env.ec2.eips)
	}
	if env.res.DeletedNATGatewayID != "nat-1" || env.res.ReleasedNATGatewayEIPID != "eipalloc-nat-1" {
		t.Errorf("result = %+v", env.res)
	}
}

func TestInitEgress_SwitchToABrokenNATInstanceKeepsTheGateway(t *testing.T) {
	fastNATTimers(t)
	shortNATGatewayWaits(t)
	nat := &fakeNATSSM{reasons: "iptables_missing", repairFixes: false}
	env := newInfraEnv(NATModeInstance, nat).
		withNATGateway("nat-1", ec2types.NatGatewayStateAvailable).
		withRouteTables(ec2types.Route{NatGatewayId: aws.String("nat-1")})

	_, err := env.svc.initEgress(context.Background(), env.res)
	if err == nil || !strings.Contains(err.Error(), "forwarding") {
		t.Fatalf("initEgress = %v, want the forwarding failure", err)
	}
	if got := env.privateRoute(t); got != "nat-1" {
		t.Errorf("private route -> %q; it must stay on the NAT gateway", got)
	}
	if env.ec2.has("DeleteNatGateway") || env.ec2.has("ReleaseAddress") {
		t.Errorf("the NAT gateway must stay; calls: %v", env.ec2.calls)
	}
}

func TestInitEgress_InstanceSteadyStateChecksForwardingLater(t *testing.T) {
	fastNATTimers(t)
	env := newInfraEnv(NATModeInstance, nil).
		withNATInstance("i-nat", ec2types.InstanceStateNameRunning).
		withRouteTables(ec2types.Route{NetworkInterfaceId: aws.String("eni-i-nat")})

	checked, err := env.svc.initEgress(context.Background(), env.res)
	if err != nil {
		t.Fatalf("initEgress: %v", err)
	}
	if checked {
		t.Error("without a switch the forwarding check stays at the end of init (Issue #87)")
	}
	for _, c := range []string{"SendCommand", "ReplaceRoute", "CreateRoute", "RunInstances", "DeleteNatGateway", "ReleaseAddress"} {
		if env.ec2.has(c) {
			t.Errorf("unexpected %s; calls: %v", c, env.ec2.calls)
		}
	}
}

func TestInitEgress_UnknownMode(t *testing.T) {
	t.Parallel()
	env := newInfraEnv("regional", nil)
	if _, err := env.svc.initEgress(context.Background(), env.res); err == nil || !strings.Contains(err.Error(), "unknown NAT mode") {
		t.Fatalf("initEgress = %v, want an unknown-mode error", err)
	}
}

func TestReleaseNATGatewayEIPs(t *testing.T) {
	t.Parallel()
	env := newInfraEnv(NATModeGateway, nil)
	env.ec2.eips = []*ec2types.Address{
		{AllocationId: aws.String("eipalloc-keep")},
		{AllocationId: aws.String("eipalloc-held"), AssociationId: aws.String("eipassoc-1")},
		{AllocationId: aws.String("eipalloc-free")},
	}
	released, err := env.svc.releaseNATGatewayEIPs(context.Background(), "eipalloc-keep")
	if err != nil {
		t.Fatalf("releaseNATGatewayEIPs: %v", err)
	}
	if !slices.Equal(released, []string{"eipalloc-free"}) {
		t.Errorf("released %v, want only the free one", released)
	}
	if len(env.ec2.eips) != 2 {
		t.Errorf("addresses left: %d, want the kept and the associated one", len(env.ec2.eips))
	}
}

// recordingIAM answers initIAM for an empty account: nothing exists, and
// every role and instance profile it creates is recorded.
type recordingIAM struct {
	awsx.IAMAPI
	createdRoles, createdProfiles []string
}

func newRecordingIAM() *recordingIAM { return &recordingIAM{} }

func (f *recordingIAM) GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "NoSuchEntity"}
}

func (f *recordingIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.createdRoles = append(f.createdRoles, aws.ToString(in.RoleName))
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{Arn: aws.String("arn:aws:iam::111122223333:role/" + aws.ToString(in.RoleName))}}, nil
}

func (f *recordingIAM) AttachRolePolicy(context.Context, *iam.AttachRolePolicyInput, ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *recordingIAM) PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *recordingIAM) GetInstanceProfile(context.Context, *iam.GetInstanceProfileInput, ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "NoSuchEntity"}
}

func (f *recordingIAM) CreateInstanceProfile(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	f.createdProfiles = append(f.createdProfiles, aws.ToString(in.InstanceProfileName))
	return &iam.CreateInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		Arn: aws.String("arn:aws:iam::111122223333:instance-profile/" + aws.ToString(in.InstanceProfileName)),
	}}, nil
}

func (f *recordingIAM) AddRoleToInstanceProfile(context.Context, *iam.AddRoleToInstanceProfileInput, ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func TestInitIAM_GatewayModeCreatesNoNATRole(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode         string
		wantRoles    []string
		wantProfiles []string
	}{
		{NATModeGateway, []string{naming.InstanceRole()}, []string{naming.InstanceProfile()}},
		{NATModeInstance,
			[]string{naming.InstanceRole(), naming.NATInstanceRole()},
			[]string{naming.InstanceProfile(), naming.NATInstanceProfile()}},
	} {
		f := newRecordingIAM()
		s := &Service{clients: &awsx.Clients{IAM: f}}
		res := &InitResult{NATMode: c.mode}
		if err := s.initIAM(context.Background(), res); err != nil {
			t.Fatalf("%s: initIAM: %v", c.mode, err)
		}
		if !slices.Equal(f.createdRoles, c.wantRoles) || !slices.Equal(f.createdProfiles, c.wantProfiles) {
			t.Errorf("%s: created roles %v / profiles %v, want %v / %v",
				c.mode, f.createdRoles, f.createdProfiles, c.wantRoles, c.wantProfiles)
		}
		if (res.NATInstanceRoleARN != "") != (c.mode == NATModeInstance) {
			t.Errorf("%s: NAT role ARN = %q", c.mode, res.NATInstanceRoleARN)
		}
	}
}
