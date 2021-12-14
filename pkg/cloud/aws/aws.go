package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/google/uuid"
	"github.com/minectl/pkg/automation"
	"github.com/minectl/pkg/common"
	minctlTemplate "github.com/minectl/pkg/template"
	"github.com/minectl/pkg/update"
	"github.com/pkg/errors"
)

const instanceNameTag = "Name"

type Aws struct {
	client *ec2.EC2
	tmpl   *minctlTemplate.Template
	region string
}

// NewAWS creates an Aws and initialises an EC2 client
func NewAWS(region, accessKey, secretKey, token string) (*Aws, error) {
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Credentials: credentials.NewStaticCredentials(accessKey, secretKey, token),
	})
	if err != nil {
		return nil, err
	}

	ec2Svc := ec2.New(sess)

	tmpl, err := minctlTemplate.NewTemplateCloudConfig()
	if err != nil {
		return nil, err
	}

	return &Aws{
		client: ec2Svc,
		region: region,
		tmpl:   tmpl,
	}, err
}

func (a *Aws) ListServer() ([]automation.RessourceResults, error) {
	var result []automation.RessourceResults
	var nextToken *string

	for {
		input := &ec2.DescribeInstancesInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String(fmt.Sprintf("tag:%s", common.InstanceTag)),
					Values: []*string{aws.String("true")},
				},
			},
			NextToken: nextToken,
		}

		instances, err := a.client.DescribeInstances(input)
		if err != nil {
			return nil, err
		}

		for _, r := range instances.Reservations {
			for _, i := range r.Instances {
				if *i.State.Name != ec2.InstanceStateNameTerminated {
					arr := automation.RessourceResults{
						ID:     *i.InstanceId,
						Region: a.region,
					}

					if i.PublicIpAddress != nil {
						arr.PublicIP = *i.PublicIpAddress
					}

					var tags []string
					var instanceName string
					for _, v := range i.Tags {
						tags = append(tags, fmt.Sprintf("%s=%s", *v.Key, *v.Value))
						if *v.Key == instanceNameTag {
							instanceName = *v.Value
						}
					}

					arr.Tags = strings.Join(tags, ",")
					arr.Name = instanceName

					result = append(result, arr)
				}
			}
		}

		nextToken = instances.NextToken
		if nextToken == nil {
			break
		}
	}

	return result, nil
}

// CreateServer TODO: https://github.com/dirien/minectl/issues/298
func (a *Aws) CreateServer(args automation.ServerArgs) (*automation.RessourceResults, error) { // nolint: gocyclo
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	image, err := a.lookupAMI("ubuntu/images/hvm-ssd/ubuntu-focal-20.04-amd64-server-20210621")
	if err != nil {
		return nil, err
	}

	pubKeyFile, err := os.ReadFile(fmt.Sprintf("%s.pub", args.MinecraftResource.GetSSH()))
	if err != nil {
		return nil, err
	}

	key, err := a.client.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(fmt.Sprintf("%s-ssh", args.MinecraftResource.GetName())),
		PublicKeyMaterial: pubKeyFile,
	})
	if err != nil {
		return nil, err
	}

	vpc, err := a.client.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("172.16.0.0/16"),
	})
	if err != nil {
		return nil, err
	}

	subnet, err := a.client.CreateSubnet(&ec2.CreateSubnetInput{
		CidrBlock: aws.String("172.16.10.0/24"),
		VpcId:     vpc.Vpc.VpcId,
	})
	if err != nil {
		return nil, err
	}

	internetGateway, err := a.client.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	if err != nil {
		return nil, err
	}

	_, err = a.client.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		VpcId:             vpc.Vpc.VpcId,
		InternetGatewayId: internetGateway.InternetGateway.InternetGatewayId,
	})
	if err != nil {
		return nil, err
	}

	routeTable, err := a.client.CreateRouteTable(&ec2.CreateRouteTableInput{
		VpcId: vpc.Vpc.VpcId,
	})
	if err != nil {
		return nil, err
	}
	_, err = a.client.CreateRoute(&ec2.CreateRouteInput{
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            internetGateway.InternetGateway.InternetGatewayId,
		RouteTableId:         routeTable.RouteTable.RouteTableId,
	})
	if err != nil {
		return nil, err
	}
	_, err = a.client.AssociateRouteTable(&ec2.AssociateRouteTableInput{
		SubnetId:     subnet.Subnet.SubnetId,
		RouteTableId: routeTable.RouteTable.RouteTableId,
	})
	if err != nil {
		return nil, err
	}
	instanceInput := &ec2.RunInstancesInput{
		ImageId:      image,
		KeyName:      key.KeyName,
		InstanceType: aws.String(args.MinecraftResource.GetSize()),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),

		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("edition"),
						Value: aws.String(args.MinecraftResource.GetEdition()),
					},
					{
						Key:   aws.String(instanceNameTag),
						Value: aws.String(args.MinecraftResource.GetName()),
					},
					{
						Key:   aws.String(common.InstanceTag),
						Value: aws.String("true"),
					},
				},
			},
		},
	}

	if args.MinecraftResource.GetVolumeSize() > 0 {
		instanceInput.BlockDeviceMappings = []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(int64(args.MinecraftResource.Spec.Server.VolumeSize)),
				},
			},
		}
	}

	userData, err := a.tmpl.GetTemplate(args.MinecraftResource, &minctlTemplate.CreateUpdateTemplateArgs{Name: minctlTemplate.GetTemplateCloudConfigName(args.MinecraftResource.IsProxyServer())})
	if err != nil {
		return nil, err
	}
	instanceInput.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(userData)))

	var secGroups []*string
	var groupID *string
	if args.MinecraftResource.GetEdition() == "bedrock" || args.MinecraftResource.GetEdition() == "nukkit" || args.MinecraftResource.GetEdition() == "powernukkit" {
		groupID, err = a.createEC2SecurityGroup(vpc.Vpc.VpcId, "udp", args.MinecraftResource.GetPort())
		if err != nil {
			return nil, err
		}
	} else {
		groupID, err = a.createEC2SecurityGroup(vpc.Vpc.VpcId, "tcp", args.MinecraftResource.GetPort())
		if err != nil {
			return nil, err
		}
		if args.MinecraftResource.HasRCON() {
			rconID, err := a.createEC2SecurityGroup(vpc.Vpc.VpcId, "tcp", args.MinecraftResource.GetRCONPort())
			if err != nil {
				return nil, err
			}
			secGroups = append(secGroups, rconID)
		}
	}
	secGroups = append(secGroups, groupID)
	if args.MinecraftResource.HasMonitoring() {
		promGroupID, err := a.createEC2SecurityGroup(vpc.Vpc.VpcId, "tcp", 9090)
		if err != nil {
			return nil, err
		}
		secGroups = append(secGroups, promGroupID)
	}
	sshGroupID, err := a.createEC2SecurityGroup(vpc.Vpc.VpcId, "tcp", 22)
	if err != nil {
		return nil, err
	}
	secGroups = append(secGroups, sshGroupID)

	instanceInput.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
		{
			Description:              aws.String("the primary device eth0"),
			DeviceIndex:              aws.Int64(0),
			AssociatePublicIpAddress: aws.Bool(true),
			SubnetId:                 subnet.Subnet.SubnetId,
			Groups:                   secGroups,
		},
	}

	result, err := a.client.RunInstances(instanceInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			return nil, aerr
		}
		return nil, err
	}

	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("timed out while creating the aws instance")
		case <-time.After(10 * time.Second):
			instanceStatus, err := a.client.DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{
				InstanceIds: aws.StringSlice([]string{*result.Instances[0].InstanceId}),
			})
			if err != nil {
				return nil, err
			}
			if *instanceStatus.InstanceStatuses[0].InstanceState.Name == "running" {
				i, err := a.client.DescribeInstances(&ec2.DescribeInstancesInput{
					InstanceIds: aws.StringSlice([]string{*result.Instances[0].InstanceId}),
				})
				if err != nil {
					return nil, err
				}
				var tags []string
				var instanceName string
				for _, v := range i.Reservations[0].Instances[0].Tags {
					tags = append(tags, fmt.Sprintf("%s=%s", *v.Key, *v.Value))

					if *v.Key == instanceNameTag {
						instanceName = *v.Value
					}
				}

				return &automation.RessourceResults{
					ID:       *i.Reservations[0].Instances[0].InstanceId,
					Name:     instanceName,
					Region:   *a.client.Config.Region,
					PublicIP: *i.Reservations[0].Instances[0].PublicIpAddress,
					Tags:     strings.Join(tags, ","),
				}, nil
			}
		}
	}
}

func (a *Aws) UpdateServer(id string, args automation.ServerArgs) error {
	i, err := a.client.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return err
	}

	remoteCommand := update.NewRemoteServer(args.MinecraftResource.GetSSH(), *i.Reservations[0].Instances[0].PublicIpAddress, "ubuntu")
	err = remoteCommand.UpdateServer(args.MinecraftResource)
	if err != nil {
		return err
	}

	return nil
}

func (a *Aws) DeleteServer(id string, args automation.ServerArgs) error {
	keys, err := a.client.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: aws.StringSlice([]string{fmt.Sprintf("%s-ssh", args.MinecraftResource.GetName())}),
	})
	if err != nil {
		return err
	}

	_, err = a.client.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: aws.String(*keys.KeyPairs[0].KeyName),
	})
	if err != nil {
		return err
	}

	i, err := a.client.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return err
	}
	// we have only on instance
	instance := i.Reservations[0].Instances[0]

	_, err = a.client.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return err
	}

	err = a.client.WaitUntilInstanceTerminated(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return err
	}

	groups := instance.SecurityGroups

	for _, group := range groups {
		_, err := a.client.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: group.GroupId,
		})
		if err != nil {
			return err
		}
	}

	vpcID := instance.VpcId
	subnetID := instance.SubnetId
	_, err = a.client.DeleteSubnet(&ec2.DeleteSubnetInput{
		SubnetId: subnetID,
	})
	if err != nil {
		return err
	}

	internetGateways, err := a.client.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("attachment.vpc-id"),
				Values: []*string{vpcID},
			},
		},
	})
	if err != nil {
		return err
	}

	for _, internetGateway := range internetGateways.InternetGateways {
		_, err := a.client.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: internetGateway.InternetGatewayId,
			VpcId:             vpcID,
		})
		if err != nil {
			return err
		}
		_, err = a.client.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: internetGateway.InternetGatewayId,
		})
		if err != nil {
			return err
		}
	}

	routeTables, err := a.client.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpcID},
			},
		},
	})
	if err != nil {
		return err
	}
	for _, routeTable := range routeTables.RouteTables {
		if routeTable.Associations == nil {
			_, err := a.client.DeleteRouteTable(&ec2.DeleteRouteTableInput{
				RouteTableId: routeTable.RouteTableId,
			})
			if err != nil {
				return err
			}
		}
	}
	_, err = a.client.DeleteVpc(&ec2.DeleteVpcInput{
		VpcId: vpcID,
	})
	if err != nil {
		return err
	}
	return nil
}

func (a *Aws) UploadPlugin(id string, args automation.ServerArgs, plugin, destination string) error {
	i, err := a.client.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return err
	}

	remoteCommand := update.NewRemoteServer(args.MinecraftResource.GetSSH(), *i.Reservations[0].Instances[0].PublicIpAddress, "ubuntu")

	err = remoteCommand.TransferFile(plugin, filepath.Join(destination, filepath.Base(plugin)))
	if err != nil {
		return err
	}

	_, err = remoteCommand.ExecuteCommand("systemctl restart minecraft.service")
	if err != nil {
		return err
	}

	return nil
}

func (a *Aws) GetServer(id string, _ automation.ServerArgs) (*automation.RessourceResults, error) {
	i, err := a.client.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice([]string{id}),
	})
	if err != nil {
		return nil, err
	}

	var tags []string
	var instanceName string
	for _, v := range i.Reservations[0].Instances[0].Tags {
		tags = append(tags, fmt.Sprintf("%s=%s", *v.Key, *v.Value))

		if *v.Key == instanceNameTag {
			instanceName = *v.Value
		}
	}

	return &automation.RessourceResults{
		ID:       *i.Reservations[0].Instances[0].InstanceId,
		Name:     instanceName,
		Region:   *a.client.Config.Region,
		PublicIP: *i.Reservations[0].Instances[0].PublicIpAddress,
		Tags:     strings.Join(tags, ","),
	}, err
}

func (a *Aws) createEC2SecurityGroup(vpcID *string, protocol string, controlPort int) (*string, error) {
	groupName := "minecraft-" + uuid.New().String()
	input := &ec2.CreateSecurityGroupInput{
		Description: aws.String("minecraft security group"),
		GroupName:   aws.String(groupName),
	}

	if vpcID != nil {
		input.VpcId = vpcID
	}

	group, err := a.client.CreateSecurityGroup(input)
	if err != nil {
		return nil, err
	}

	err = a.createEC2SecurityGroupRule(*group.GroupId, protocol, controlPort, controlPort)
	if err != nil {
		return group.GroupId, err
	}

	return group.GroupId, nil
}

func (a *Aws) createEC2SecurityGroupRule(groupID, protocol string, fromPort, toPort int) error {
	_, err := a.client.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		CidrIp:     aws.String("0.0.0.0/0"),
		FromPort:   aws.Int64(int64(fromPort)),
		IpProtocol: aws.String(protocol),
		ToPort:     aws.Int64(int64(toPort)),
		GroupId:    aws.String(groupID),
	})
	if err != nil {
		return err
	}

	return nil
}

// lookupAMI gets the AMI ID that the exit node will use
func (a *Aws) lookupAMI(name string) (*string, error) {
	images, err := a.client.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("name"),
				Values: []*string{
					aws.String(name),
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	if len(images.Images) == 0 {
		return nil, fmt.Errorf("image not found")
	}

	return images.Images[0].ImageId, nil
}
