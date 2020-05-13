package opentelekomcloud

import (
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/hashicorp/go-multierror"
	"github.com/huaweicloud/golangsdk"
	"github.com/huaweicloud/golangsdk/openstack/compute/v2/servers"
	"github.com/opentelekomcloud-infra/crutch-house/clientconfig"
	"github.com/opentelekomcloud-infra/crutch-house/services"
)

const (
	dockerPort           = 2376
	driverName           = "otc-v2"
	defaultSecurityGroup = "docker-machine-grp"
	defaultAZ            = "eu-de-01"
	defaultFlavor        = "s2.large.4"
	defaultImage         = "Standard_Ubuntu_18.04_latest"
	defaultSSHUser       = "ubuntu"
	defaultSSHPort       = 22
	defaultRegion        = "eu-de"
	defaultAuthURL       = "https://iam.eu-de.otc.t-systems.com/v3"
	defaultVpcName       = "vpc-docker-machine"
	defaultSubnetName    = "subnet-docker-machine"
	defaultVolumeSize    = 200
	defaultVolumeType    = "SSD"
	k8sGroupName         = "sg-k8s"
)

var (
	// https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/#check-required-ports
	k8sPorts = []services.PortRange{
		// control-plane node(s)
		{From: 6443},
		{From: 2379, To: 2380},
		{From: 10250, To: 10252},
		// worker node(s)
		{From: 30000, To: 32767},
	}
)

type managedSting struct {
	Value         string `json:"value"`
	DriverManaged bool   `json:"managed"`
}

// Driver for docker-machine
type Driver struct {
	*drivers.BaseDriver
	Cloud                  string             `json:"cloud,omitempty"`
	AuthURL                string             `json:"auth_url,omitempty"`
	CACert                 string             `json:"ca_cert,omitempty"`
	ValidateCert           bool               `json:"validate_cert"`
	DomainID               string             `json:"domain_id,omitempty"`
	DomainName             string             `json:"domain_name,omitempty"`
	Username               string             `json:"username,omitempty"`
	Password               string             `json:"password,omitempty"`
	ProjectName            string             `json:"project_name,omitempty"`
	ProjectID              string             `json:"project_id,omitempty"`
	Region                 string             `json:"region,omitempty"`
	AccessKey              string             `json:"access_key,omitempty"`
	SecretKey              string             `json:"secret_key,omitempty"`
	AvailabilityZone       string             `json:"-"`
	EndpointType           string             `json:"endpoint_type,omitempty"`
	InstanceID             string             `json:"instance_id"`
	FlavorName             string             `json:"-"`
	FlavorID               string             `json:"-"`
	ImageName              string             `json:"-"`
	KeyPairName            managedSting       `json:"key_pair"`
	VpcName                string             `json:"-"`
	VpcID                  managedSting       `json:"vpc_id"`
	SubnetName             string             `json:"-"`
	SubnetID               managedSting       `json:"subnet_id"`
	PrivateKeyFile         string             `json:"private_key"`
	SecurityGroups         []string           `json:"-"`
	SecurityGroupIDs       []string           `json:"-"`
	ServerGroup            string             `json:"-"`
	ServerGroupID          string             `json:"-"`
	ManagedSecurityGroup   string             `json:"-"`
	ManagedSecurityGroupID string             `json:"managed_security_group,omitempty"`
	K8sSecurityGroup       string             `json:"-"`
	K8sSecurityGroupID     string             `json:"k8s_security_group,omitempty"`
	FloatingIP             managedSting       `json:"floating_ip"`
	Token                  string             `json:"token,omitempty"`
	RootVolumeOpts         *services.DiskOpts `json:"-"`
	UserDataFile           string             `json:"-"`
	UserData               []byte             `json:"-"`
	Tags                   []string           `json:"-"`
	IPVersion              int                `json:"-"`
	skipEIPCreation        bool
	eipConfig              *services.ElasticIPOpts
	client                 services.Client
}

func (d *Driver) createVPC() error {
	if d.VpcID.Value != "" {
		return nil
	}
	vpc, err := d.client.CreateVPC(d.VpcName)
	if err != nil {
		return err
	}
	d.VpcID = managedSting{
		Value:         vpc.ID,
		DriverManaged: true,
	}
	if err := d.client.WaitForVPCStatus(d.VpcID.Value, "OK"); err != nil {
		return err
	}
	return nil
}

func (d *Driver) createSubnet() error {
	if d.SubnetID.Value != "" {
		return nil
	}
	subnet, err := d.client.CreateSubnet(d.VpcID.Value, d.SubnetName)
	if err != nil {
		return err
	}
	d.SubnetID = managedSting{
		Value:         subnet.ID,
		DriverManaged: true,
	}
	if err := d.client.WaitForSubnetStatus(d.SubnetID.Value, "ACTIVE"); err != nil {
		return err
	}
	return nil
}
func (d *Driver) createK8sGroup() error {
	if d.K8sSecurityGroupID != "" || d.K8sSecurityGroup == "" {
		return nil
	}
	sg, err := d.client.CreateSecurityGroup(d.K8sSecurityGroup, k8sPorts...)
	if err != nil {
		return err
	}
	d.K8sSecurityGroupID = sg.ID
	return nil
}

func (d *Driver) createDefaultGroup() error {
	if d.ManagedSecurityGroupID != "" || d.ManagedSecurityGroup == "" {
		return nil
	}
	sg, err := d.client.CreateSecurityGroup(d.ManagedSecurityGroup,
		services.PortRange{From: d.SSHPort},
		services.PortRange{From: dockerPort},
	)
	if err != nil {
		return err
	}
	d.ManagedSecurityGroupID = sg.ID
	return nil
}

const notFound = "%s not found by name `%s`"

// Resolve name to IDs where possible
func (d *Driver) resolveIDs() error {
	if d.VpcID.Value == "" && d.VpcName != "" {
		vpcID, err := d.client.FindVPC(d.VpcName)
		if err != nil {
			return err
		}
		d.VpcID = managedSting{Value: vpcID}
	}

	if d.SubnetID.Value == "" && d.SubnetName != "" {
		subnetID, err := d.client.FindSubnet(d.VpcID.Value, d.SubnetName)
		if err != nil {
			return err
		}
		d.SubnetID = managedSting{Value: subnetID}
	}

	if d.FlavorID == "" && d.FlavorName != "" {
		flavID, err := d.client.FindFlavor(d.FlavorName)
		if err != nil {
			return err
		}
		if flavID == "" {
			return fmt.Errorf(notFound, "flavor", d.FlavorName)
		}
		d.FlavorID = flavID
	}
	if d.RootVolumeOpts.SourceID == "" && d.ImageName != "" {
		imageID, err := d.client.FindImage(d.ImageName)
		if err != nil {
			return err
		}
		if imageID == "" {
			return fmt.Errorf(notFound, "image", d.ImageName)
		}
		d.RootVolumeOpts.SourceID = imageID
	}
	sgIDs, err := d.client.FindSecurityGroups(d.SecurityGroups)
	if err != nil {
		return err
	}
	d.SecurityGroupIDs = sgIDs

	if d.ServerGroupID == "" && d.ServerGroup != "" {
		serverGroupID, err := d.client.FindServerGroup(d.ServerGroup)
		if err != nil {
			return err
		}
		d.ServerGroupID = serverGroupID
	}

	return nil
}

func (d *Driver) createResources() error {
	// network init
	if err := d.initNetwork(); err != nil {
		return err
	}
	if err := d.initCompute(); err != nil {
	}
	if err := d.resolveIDs(); err != nil {
		return err
	}
	if err := d.createVPC(); err != nil {
		return err
	}
	if err := d.createSubnet(); err != nil {
		return err
	}
	if err := d.createDefaultGroup(); err != nil {
		return err
	}
	if err := d.createK8sGroup(); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Authenticate() error {
	if d.client != nil {
		return nil
	}
	opts := &clientconfig.ClientOpts{
		Cloud:        d.Cloud,
		RegionName:   d.Region,
		EndpointType: d.EndpointType,
		AuthInfo: &clientconfig.AuthInfo{
			AuthURL:     d.AuthURL,
			Username:    d.Username,
			Password:    d.Password,
			ProjectName: d.ProjectName,
			ProjectID:   d.ProjectID,
			DomainName:  d.DomainName,
			DomainID:    d.DomainID,
			AccessKey:   d.AccessKey,
			SecretKey:   d.SecretKey,
			Token:       d.Token,
		},
	}
	d.client = services.NewClient(opts)
	return d.client.Authenticate()
}

func (d *Driver) createFloatingIP() error {
	if d.FloatingIP.Value == "" {
		eip, err := d.client.CreateEIP(d.eipConfig)
		if err != nil {
			return err
		}
		if err := d.client.WaitForEIPActive(eip.ID); err != nil {
			return err
		}
		d.FloatingIP = managedSting{Value: eip.PublicAddress, DriverManaged: true}
	}
	if err := d.client.BindFloatingIP(d.FloatingIP.Value, d.InstanceID); err != nil {
		return err
	}
	return nil
}

func (d *Driver) useLocalIP() error {
	instance, err := d.client.GetInstanceStatus(d.InstanceID)
	if err != nil {
		return err
	}
	for _, addrPool := range instance.Addresses {
		addrDetails := addrPool.([]interface{})[0].(map[string]interface{})
		d.FloatingIP = managedSting{
			Value:         addrDetails["addr"].(string),
			DriverManaged: false,
		}
		return nil
	}
	return nil
}

// Create creates new ECS used for docker-machine
func (d *Driver) Create() error {
	if err := d.Authenticate(); err != nil {
		return err
	}
	if err := d.createResources(); err != nil {
		return err
	}
	if d.KeyPairName.Value != "" {
		if err := d.loadSSHKey(); err != nil {
			return err
		}
	} else {
		d.KeyPairName = managedSting{
			fmt.Sprintf("%s-%s", d.MachineName, mcnutils.GenerateRandomID()),
			true,
		}
		if err := d.createSSHKey(); err != nil {
			return err
		}
	}
	if err := d.createInstance(); err != nil {
		return err
	}
	if d.skipEIPCreation {
		if err := d.useLocalIP(); err != nil {
			return err
		}
	} else {
		if err := d.createFloatingIP(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) getUserData() error {
	if d.UserDataFile == "" || len(d.UserData) != 0 {
		return nil
	}
	userData, err := ioutil.ReadFile(d.UserDataFile)
	if err != nil {
		return err
	}
	d.UserData = userData
	return nil
}

func (d *Driver) createInstance() error {
	if d.InstanceID != "" {
		return nil
	}
	if err := d.initCompute(); err != nil {
		return err
	}
	secGroups := d.SecurityGroupIDs
	if d.ManagedSecurityGroupID != "" {
		secGroups = append(secGroups, d.ManagedSecurityGroupID)
	}
	if d.K8sSecurityGroupID != "" {
		secGroups = append(secGroups, d.K8sSecurityGroupID)
	}

	serverOpts := &services.ExtendedServerOpts{
		CreateOpts: &servers.CreateOpts{
			Name:             d.MachineName,
			FlavorRef:        d.FlavorID,
			SecurityGroups:   secGroups,
			AvailabilityZone: d.AvailabilityZone,
		},
		SubnetID:      d.SubnetID.Value,
		KeyPairName:   d.KeyPairName.Value,
		DiskOpts:      d.RootVolumeOpts,
		ServerGroupID: d.ServerGroupID,
	}

	if err := d.getUserData(); err != nil {
		return err
	}
	serverOpts.UserData = d.UserData

	instance, err := d.client.CreateInstance(serverOpts)
	if err != nil {
		return err
	}
	d.InstanceID = instance.ID

	if len(d.Tags) > 0 {
		if err := d.client.AddTags(d.InstanceID, d.Tags); err != nil {
			return err
		}
	}

	if err := d.client.WaitForInstanceStatus(d.InstanceID, services.InstanceStatusRunning); err != nil {
		return err
	}
	return nil
}

func (d *Driver) DriverName() string {
	return driverName
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			Name:   "otc-cloud",
			EnvVar: "OS_CLOUD",
			Usage:  "Name of cloud in `clouds.yaml` file",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-auth-url",
			EnvVar: "OS_AUTH_URL",
			Usage:  "OpenTelekomCloud authentication URL",
			Value:  defaultAuthURL,
		},
		mcnflag.StringFlag{
			Name:   "otc-cacert",
			EnvVar: "OS_CACERT",
			Usage:  "CA certificate bundle to verify against",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-domain-id",
			EnvVar: "OS_DOMAIN_ID",
			Usage:  "OpenTelekomCloud domain ID",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-domain-name",
			EnvVar: "OS_DOMAIN_NAME",
			Usage:  "OpenTelekomCloud domain name",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-username",
			EnvVar: "OS_USERNAME",
			Usage:  "OpenTelekomCloud username",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-password",
			EnvVar: "OS_PASSWORD",
			Usage:  "OpenTelekomCloud password",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:   "otc-project-name",
			EnvVar: "OS_PROJECT_NAME",
			Usage:  "OpenTelekomCloud project name",
		},
		mcnflag.StringFlag{
			Name:   "otc-project-id",
			EnvVar: "OS_PROJECT_ID",
			Usage:  "OpenTelekomCloud project ID",
		},
		mcnflag.StringFlag{
			Name:   "otc-tenant-id",
			Usage:  "OpenTelekomCloud project ID. DEPRECATED: use -otc-project-id instead",
			EnvVar: "TENANT_ID",
		},
		mcnflag.StringFlag{
			Name:   "otc-region",
			EnvVar: "REGION",
			Usage:  "OpenTelekomCloud region name",
			Value:  defaultRegion,
		},
		mcnflag.StringFlag{
			Name:   "otc-access-key-id",
			Usage:  "OpenTelekomCloud access key ID for AK/SK auth",
			EnvVar: "ACCESS_KEY_ID",
		},
		mcnflag.StringFlag{
			Name:   "otc-access-key-key",
			Usage:  "OpenTelekomCloud secret access key for AK/SK auth",
			EnvVar: "ACCESS_KEY_SECRET",
		},
		mcnflag.StringFlag{
			Name:   "otc-availability-zone",
			EnvVar: "OS_AVAILABILITY_ZONE",
			Usage:  "OpenTelekomCloud availability zone",
			Value:  defaultAZ,
		},
		mcnflag.StringFlag{
			Name:   "otc-available-zone",
			EnvVar: "AVAILABLE_ZONE",
			Usage:  "OpenTelekomCloud availability zone. DEPRECATED: use -otc-availability-zone instead",
		},
		mcnflag.StringFlag{
			Name:   "otc-flavor-id",
			EnvVar: "FLAVOR_ID",
			Usage:  "OpenTelekomCloud flavor id to use for the instance",
		},
		mcnflag.StringFlag{
			Name:   "otc-flavor-name",
			EnvVar: "OS_FLAVOR_NAME",
			Usage:  "OpenTelekomCloud flavor name to use for the instance",
			Value:  defaultFlavor,
		},
		mcnflag.StringFlag{
			Name:   "otc-image-id",
			EnvVar: "IMAGE_ID",
			Usage:  "OpenTelekomCloud image id to use for the instance",
		},
		mcnflag.StringFlag{
			Name:   "otc-image-name",
			EnvVar: "OS_IMAGE_NAME",
			Usage:  "OpenTelekomCloud image name to use for the instance",
			Value:  defaultImage,
		},
		mcnflag.StringFlag{
			Name:   "otc-keypair-name",
			EnvVar: "OS_KEYPAIR_NAME",
			Usage:  "OpenTelekomCloud keypair to use to SSH to the instance",
		},
		mcnflag.StringFlag{
			Name:   "otc-vpc-id",
			EnvVar: "VPC_ID",
			Usage:  "OpenTelekomCloud VPC id the machine will be connected on",
		},
		mcnflag.StringFlag{
			Name:   "otc-vpc-name",
			EnvVar: "OS_VPC_NAME",
			Usage:  "OpenTelekomCloud VPC name the machine will be connected on",
			Value:  defaultVpcName,
		},
		mcnflag.StringFlag{
			Name:   "otc-subnet-id",
			EnvVar: "SUBNET_ID",
			Usage:  "OpenTelekomCloud subnet id the machine will be connected on",
		},
		mcnflag.StringFlag{
			Name:   "otc-subnet-name",
			EnvVar: "OS_SUBNET_NAME",
			Usage:  "OpenTelekomCloud subnet name the machine will be connected on",
			Value:  defaultSubnetName,
		},
		mcnflag.StringFlag{
			Name:   "otc-private-key-file",
			EnvVar: "OS_PRIVATE_KEY_FILE",
			Usage:  "Private key file to use for SSH (absolute path)",
		},
		mcnflag.StringFlag{
			Name:   "otc-user-data-file",
			EnvVar: "OS_USER_DATA_FILE",
			Usage:  "File containing an user data script",
		},
		mcnflag.StringFlag{
			Name:  "otc-user-data-raw",
			Usage: "Contents of user data file as a string",
		},
		mcnflag.StringFlag{
			Name:   "otc-token",
			EnvVar: "OS_TOKEN",
			Usage:  "OpenTelekomCloud authorization token",
		},
		mcnflag.StringFlag{
			Name:   "otc-sec-groups",
			EnvVar: "OS_SECURITY_GROUP",
			Usage:  "Existing security groups to use, separated by comma",
		},
		mcnflag.StringFlag{
			Name:   "otc-floating-ip",
			EnvVar: "OS_FLOATING_IP",
			Usage:  "OpenTelekomCloud floating IP to use",
		},
		mcnflag.StringFlag{
			Name:   "otc-floating-ip-type",
			EnvVar: "OS_FLOATING_IP_TYPE",
			Usage:  "OpenTelekomCloud bandwidth type",
			Value:  "5_bgp",
		},
		mcnflag.StringFlag{
			Name:   "otc-elastic-ip-type",
			EnvVar: "ELASTICIP_TYPE",
			Usage:  "OpenTelekomCloud bandwidth type. DEPRECATED! Use -otc-floating-ip-type instead",
		},
		mcnflag.IntFlag{
			Name:   "otc-bandwidth-size",
			EnvVar: "BANDWIDTH_SIZE",
			Usage:  "OpenTelekomCloud bandwidth size",
			Value:  100,
		},
		mcnflag.StringFlag{
			Name:   "otc-bandwidth-type",
			EnvVar: "BANDWIDTH_TYPE",
			Usage:  "OpenTelekomCloud bandwidth share type",
			Value:  "PER",
		},
		mcnflag.IntFlag{
			Name:   "otc-elastic-ip",
			EnvVar: "ELASTIC_IP",
			Usage:  "If set to 0, elastic IP won't be created. DEPRECATED: use -otc-skip-ip instead",
			Value:  1,
		},
		mcnflag.BoolFlag{
			Name:  "otc-skip-ip",
			Usage: "If set, elastic IP won't be created",
		},
		mcnflag.IntFlag{
			Name:   "otc-ip-version",
			EnvVar: "OS_IP_VERSION",
			Usage:  "OpenTelekomCloud version of IP address assigned for the machine",
			Value:  4,
		},
		mcnflag.StringFlag{
			Name:   "otc-ssh-user",
			EnvVar: "SSH_USER",
			Usage:  "Machine SSH username",
			Value:  defaultSSHUser,
		},
		mcnflag.IntFlag{
			Name:   "otc-ssh-port",
			EnvVar: "OS_SSH_PORT",
			Usage:  "Machine SSH port",
			Value:  defaultSSHPort,
		},
		mcnflag.StringFlag{
			Name:   "otc-endpoint-type",
			EnvVar: "OS_INTERFACE",
			Usage:  "OpenTelekomCloud interface (endpoint) type",
			Value:  "public",
		},
		mcnflag.BoolFlag{
			Name:  "otc-skip-default-sg",
			Usage: "Don't create default security group",
		},
		mcnflag.BoolFlag{
			Name:  "otc-k8s-group",
			Usage: "Create security group with k8s ports allowed",
		},
		mcnflag.StringFlag{
			Name:   "otc-server-group",
			EnvVar: "OS_SERVER_GROUP",
			Usage:  "Define server group where server will be created",
		},
		mcnflag.StringFlag{
			Name:   "otc-server-group-id",
			EnvVar: "OS_SERVER_GROUP_ID",
			Usage:  "Define server group where server will be created by ID",
		},
		mcnflag.IntFlag{
			Name:   "otc-root-volume-size",
			EnvVar: "ROOT_VOLUME_SIZEROOT_VOLUME_SIZE",
			Usage:  "Set volume size of root partition",
			Value:  defaultVolumeSize,
		},
		mcnflag.StringFlag{
			Name:   "otc-tags",
			EnvVar: "OS_TAGS",
			Usage:  "Comma-separated list of instance tags",
		},
		mcnflag.StringFlag{
			Name:   "otc-root-volume-type",
			EnvVar: "ROOT_VOLUME_TYPE",
			Usage:  "Set volume type of root partition (one of SATA, SAS, SSD)",
			Value:  defaultVolumeType,
		},
	}
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = defaultSSHPort
	}
	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = defaultSSHUser
	}
	return d.SSHUser
}

func (d *Driver) GetIP() (string, error) {
	d.IPAddress = d.FloatingIP.Value
	return d.BaseDriver.GetIP()
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil || ip == "" {
		return "", err
	}
	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, strconv.Itoa(dockerPort))), nil
}

func (d *Driver) GetState() (state.State, error) {
	if err := d.initCompute(); err != nil {
		return state.None, err
	}
	instance, err := d.client.GetInstanceStatus(d.InstanceID)
	if err != nil {
		return state.None, err
	}
	switch instance.Status {
	case services.InstanceStatusRunning:
		return state.Running, nil
	case "PAUSED":
		return state.Paused, nil
	case services.InstanceStatusStopped:
		return state.Stopped, nil
	case "BUILDING":
		return state.Starting, nil
	case "ERROR":
		return state.Error, nil
	default:
		return state.None, nil
	}
}

func (d *Driver) Start() error {
	if err := d.initCompute(); err != nil {
		return err
	}
	if err := d.client.StartInstance(d.InstanceID); err != nil {
		return err
	}
	return d.client.WaitForInstanceStatus(d.InstanceID, services.InstanceStatusRunning)
}

func (d *Driver) Stop() error {
	if err := d.initCompute(); err != nil {
		return err
	}
	if err := d.client.StopInstance(d.InstanceID); err != nil {
		return err
	}
	return d.client.WaitForInstanceStatus(d.InstanceID, services.InstanceStatusStopped)
}

func (d *Driver) Kill() error {
	return d.Stop()
}

func (d *Driver) deleteInstance() error {
	if err := d.initCompute(); err != nil {
		return err
	}
	if err := d.client.DeleteInstance(d.InstanceID); err != nil {
		return err
	}
	err := d.client.WaitForInstanceStatus(d.InstanceID, "")
	switch err.(type) {
	case golangsdk.ErrDefault404:
	default:
		return err
	}
	return nil
}

func (d *Driver) deleteSubnet() error {
	if err := d.initNetwork(); err != nil {
		return err
	}
	if d.SubnetID.DriverManaged {
		err := d.client.DeleteSubnet(d.VpcID.Value, d.SubnetID.Value)
		if err != nil {
			return err
		}
		err = d.client.WaitForSubnetStatus(d.SubnetID.Value, "")
		switch err.(type) {
		case golangsdk.ErrDefault404:
		default:
			return err
		}
	}
	return nil
}

func (d *Driver) deleteVPC() error {
	if err := d.initNetwork(); err != nil {
		return err
	}
	if d.VpcID.DriverManaged {
		err := d.client.DeleteVPC(d.VpcID.Value)
		if err != nil {
			return err
		}
		err = d.client.WaitForVPCStatus(d.VpcID.Value, "")
		switch err.(type) {
		case golangsdk.ErrDefault404:
		default:
			return err
		}
	}
	return nil
}

func (d *Driver) deleteSecGroups() error {
	if err := d.initCompute(); err != nil {
		return err
	}
	for _, id := range []string{d.ManagedSecurityGroupID, d.K8sSecurityGroupID} {
		if id == "" {
			continue
		}
		if err := d.client.DeleteSecurityGroup(id); err != nil {
			return err
		}
		if err := d.client.WaitForGroupDeleted(id); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) Remove() error {
	var errs error
	if err := d.Authenticate(); err != nil {
		return err
	}
	if err := d.deleteInstance(); err != nil {
		errs = multierror.Append(errs, err)
	}
	if d.KeyPairName.DriverManaged {
		if err := d.client.DeleteKeyPair(d.KeyPairName.Value); err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	if d.FloatingIP.DriverManaged && d.FloatingIP.Value != "" {
		if err := d.client.DeleteFloatingIP(d.FloatingIP.Value); err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	if err := d.deleteSubnet(); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := d.deleteSecGroups(); err != nil {
		errs = multierror.Append(errs, err)
	}
	if err := d.deleteVPC(); err != nil {
		errs = multierror.Append(errs, err)
	}
	return errs
}

func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

// NewDriver create new driver instance
func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			SSHUser:     defaultSSHUser,
			SSHPort:     defaultSSHPort,
			StorePath:   storePath,
		},
		client: nil,
	}
}

func (d *Driver) initCompute() error {
	if err := d.Authenticate(); err != nil {
		return err
	}
	if err := d.client.InitCompute(); err != nil {
		return err
	}
	return nil
}

func (d *Driver) initNetwork() error {
	if err := d.Authenticate(); err != nil {
		return err
	}
	if err := d.client.InitNetwork(); err != nil {
		return err
	}
	return nil
}

func (d *Driver) loadSSHKey() error {
	log.Debug("Loading Key Pair", d.KeyPairName.Value)
	if err := d.initCompute(); err != nil {
		return err
	}
	log.Debug("Loading Private Key from", d.PrivateKeyFile)
	privateKey, err := ioutil.ReadFile(d.PrivateKeyFile)
	if err != nil {
		return err
	}
	publicKey, err := d.client.GetPublicKey(d.KeyPairName.Value)
	if err != nil {
		return err
	}
	privateKeyPath := d.GetSSHKeyPath()
	if err := ioutil.WriteFile(privateKeyPath, privateKey, 0600); err != nil {
		return err
	}
	if err := ioutil.WriteFile(privateKeyPath+".pub", publicKey, 0600); err != nil {
		return err
	}

	return nil
}

func (d *Driver) createKeyPair(publicKey []byte) (string, error) {
	kp, err := d.client.CreateKeyPair(d.KeyPairName.Value, string(publicKey))
	if err != nil {
		return "", err
	}
	return kp.PublicKey, nil
}

func (d *Driver) createSSHKey() error {
	d.KeyPairName.Value = strings.Replace(d.KeyPairName.Value, ".", "_", -1)
	log.Debug("Creating Key Pair...", map[string]string{"Name": d.KeyPairName.Value})
	keyPath := d.GetSSHKeyPath()
	if err := ssh.GenerateSSHKey(keyPath); err != nil {
		return err
	}
	d.PrivateKeyFile = keyPath
	publicKey, err := ioutil.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}
	d.KeyPairName = managedSting{d.KeyPairName.Value, true}
	if err := d.initCompute(); err != nil {
		return err
	}
	if _, err := d.createKeyPair(publicKey); err != nil {
		return err
	}
	return nil
}

// SetConfigFromFlags loads driver configuration from given flags
func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.AuthURL = flags.String("otc-auth-url")
	d.Cloud = flags.String("otc-cloud")
	d.CACert = flags.String("otc-cacert")
	d.DomainID = flags.String("otc-domain-id")
	d.DomainName = flags.String("otc-domain-name")
	d.Username = flags.String("otc-username")
	d.Password = flags.String("otc-password")
	d.ProjectName = flags.String("otc-project-name")
	projectID := flags.String("otc-tenant-id")
	if projectID == "" {
		projectID = flags.String("otc-project-id")
	}
	d.ProjectID = projectID
	d.Region = flags.String("otc-region")
	d.EndpointType = flags.String("otc-endpoint-type")
	d.FlavorID = flags.String("otc-flavor-id")
	d.FlavorName = flags.String("otc-flavor-name")
	d.ImageName = flags.String("otc-image-name")
	d.VpcID = managedSting{Value: flags.String("otc-vpc-id")}
	d.VpcName = flags.String("otc-vpc-name")
	d.SubnetID = managedSting{Value: flags.String("otc-subnet-id")}
	d.SubnetName = flags.String("otc-subnet-name")
	d.FloatingIP = managedSting{Value: flags.String("otc-floating-ip")}
	d.IPVersion = flags.Int("otc-ip-version")
	d.SSHUser = flags.String("otc-ssh-user")
	d.SSHPort = flags.Int("otc-ssh-port")
	d.KeyPairName = managedSting{Value: flags.String("otc-keypair-name")}
	d.PrivateKeyFile = flags.String("otc-private-key-file")
	d.Token = flags.String("otc-token")
	d.UserDataFile = flags.String("otc-user-data-file")
	d.UserData = []byte(flags.String("otc-user-data-raw"))
	d.ServerGroup = flags.String("otc-server-group")
	d.ServerGroupID = flags.String("otc-server-group-id")
	tags := flags.String("otc-tags")
	if tags != "" {
		d.Tags = strings.Split(tags, ",")
	}
	d.AccessKey = flags.String("otc-access-key-id")
	d.SecretKey = flags.String("otc-access-key-key")

	d.RootVolumeOpts = &services.DiskOpts{
		SourceID: flags.String("otc-image-id"),
		Size:     flags.Int("otc-root-volume-size"),
		Type:     flags.String("otc-root-volume-type"),
	}
	ipType := flags.String("otc-elastic-ip-type")
	if ipType == "" {
		ipType = flags.String("otc-floating-ip-type")
	}

	d.eipConfig = &services.ElasticIPOpts{
		IPType:        ipType,
		BandwidthSize: flags.Int("otc-bandwidth-size"),
		BandwidthType: flags.String("otc-bandwidth-type"),
	}
	d.skipEIPCreation = flags.Int("otc-elastic-ip") == 0 || flags.Bool("otc-skip-ip")

	az := flags.String("otc-available-zone")
	if az == "" {
		az = flags.String("otc-availability-zone")
	}
	d.AvailabilityZone = az

	if sg := flags.String("otc-sec-groups"); sg != "" {
		d.SecurityGroups = strings.Split(sg, ",")
	}

	if !flags.Bool("otc-skip-default-sg") {
		d.ManagedSecurityGroup = defaultSecurityGroup
	}

	if flags.Bool("otc-k8s-group") {
		d.K8sSecurityGroup = k8sGroupName
	}

	d.SetSwarmConfigFromFlags(flags)
	return d.checkConfig()
}

const errorBothOptions = "both %s and %s must be specified"

func (d *Driver) checkConfig() error {
	if (d.KeyPairName.Value != "" && d.PrivateKeyFile == "") || (d.KeyPairName.Value == "" && d.PrivateKeyFile != "") {
		return fmt.Errorf(errorBothOptions, "KeyPairName", "PrivateKeyFile")
	}
	if d.Cloud == "" &&
		(d.Username == "" || d.Password == "") &&
		d.Token == "" &&
		(d.AccessKey == "" || d.SecretKey == "") {
		return fmt.Errorf("at least one authorization method must be provided")
	}
	if len(d.UserData) > 0 && d.UserDataFile != "" {
		return fmt.Errorf("both `-otc-user-data` and `-otc-user-data` is defined")
	}
	return nil
}
