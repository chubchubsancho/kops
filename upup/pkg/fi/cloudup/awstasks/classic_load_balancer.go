/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package awstasks

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	elb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing/types"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/route53"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/wellknownservices"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"
	"k8s.io/kops/util/pkg/slice"
)

// LoadBalancer manages an ELB.  We find the existing ELB using the Name tag.

var _ DNSTarget = &ClassicLoadBalancer{}

// +kops:fitask
type ClassicLoadBalancer struct {
	// We use the Name tag to find the existing ELB, because we are (more or less) unrestricted when
	// it comes to tag values, but the LoadBalancerName is length limited
	Name      *string
	Lifecycle fi.Lifecycle

	// LoadBalancerName is the name in ELB, possibly different from our name
	// (ELB is restricted as to names, so we have limited choices!)
	// We use the Name tag to find the existing ELB.
	LoadBalancerName *string

	DNSName      *string
	HostedZoneId *string

	Subnets        []*Subnet
	SecurityGroups []*SecurityGroup

	Listeners map[string]*ClassicLoadBalancerListener

	Scheme *string

	HealthCheck            *ClassicLoadBalancerHealthCheck
	AccessLog              *ClassicLoadBalancerAccessLog
	ConnectionDraining     *ClassicLoadBalancerConnectionDraining
	ConnectionSettings     *ClassicLoadBalancerConnectionSettings
	CrossZoneLoadBalancing *ClassicLoadBalancerCrossZoneLoadBalancing
	SSLCertificateID       string

	Tags map[string]string

	// Shared is set if this is an external LB (one we don't create or own)
	Shared *bool

	// WellKnownServices indicates which services are supported by this resource.
	// This field is internal and is not rendered to the cloud.
	WellKnownServices []wellknownservices.WellKnownService
}

var _ fi.CompareWithID = &ClassicLoadBalancer{}
var _ fi.CloudupTaskNormalize = &ClassicLoadBalancer{}

func (e *ClassicLoadBalancer) CompareWithID() *string {
	return e.Name
}

type ClassicLoadBalancerListener struct {
	InstancePort     int32
	SSLCertificateID string
}

func (e *ClassicLoadBalancerListener) mapToAWS(loadBalancerPort int32) elbtypes.Listener {
	l := elbtypes.Listener{
		LoadBalancerPort: loadBalancerPort,
		InstancePort:     aws.Int32(e.InstancePort),
	}

	if e.SSLCertificateID != "" {
		l.Protocol = aws.String("SSL")
		l.InstanceProtocol = aws.String("SSL")
		l.SSLCertificateId = aws.String(e.SSLCertificateID)
	} else {
		l.Protocol = aws.String("TCP")
		l.InstanceProtocol = aws.String("TCP")
	}

	return l
}

var _ fi.CloudupHasDependencies = &ClassicLoadBalancerListener{}

func (e *ClassicLoadBalancerListener) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	return nil
}

func findLoadBalancerByLoadBalancerName(ctx context.Context, cloud awsup.AWSCloud, loadBalancerName string) (*elbtypes.LoadBalancerDescription, error) {
	request := &elb.DescribeLoadBalancersInput{
		LoadBalancerNames: []string{loadBalancerName},
	}
	found, err := describeLoadBalancers(ctx, cloud, request, func(lb elbtypes.LoadBalancerDescription) bool {
		// TODO: Filter by cluster?

		if aws.ToString(lb.LoadBalancerName) == loadBalancerName {
			return true
		}

		klog.Warningf("Got ELB with unexpected name: %q", aws.ToString(lb.LoadBalancerName))
		return false
	})
	if err != nil {
		if awsError, ok := err.(awserr.Error); ok {
			if awsError.Code() == "LoadBalancerNotFound" {
				return nil, nil
			}
		}

		return nil, fmt.Errorf("error listing ELBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple ELBs with name %q", loadBalancerName)
	}

	return &found[0], nil
}

func findLoadBalancerByAlias(cloud awsup.AWSCloud, alias *route53.AliasTarget) (*elbtypes.LoadBalancerDescription, error) {
	ctx := context.TODO()
	// TODO: Any way to avoid listing all ELBs?
	request := &elb.DescribeLoadBalancersInput{}

	dnsName := aws.ToString(alias.DNSName)
	matchDnsName := strings.TrimSuffix(dnsName, ".")
	if matchDnsName == "" {
		return nil, fmt.Errorf("DNSName not set on AliasTarget")
	}

	matchHostedZoneId := aws.ToString(alias.HostedZoneId)

	found, err := describeLoadBalancers(ctx, cloud, request, func(lb elbtypes.LoadBalancerDescription) bool {
		// TODO: Filter by cluster?

		if matchHostedZoneId != aws.ToString(lb.CanonicalHostedZoneNameID) {
			return false
		}

		lbDnsName := aws.ToString(lb.DNSName)
		lbDnsName = strings.TrimSuffix(lbDnsName, ".")
		return lbDnsName == matchDnsName || "dualstack."+lbDnsName == matchDnsName
	})
	if err != nil {
		return nil, fmt.Errorf("error listing ELBs: %v", err)
	}

	if len(found) == 0 {
		return nil, nil
	}

	if len(found) != 1 {
		return nil, fmt.Errorf("Found multiple ELBs with DNSName %q", dnsName)
	}

	return &found[0], nil
}

func describeLoadBalancers(ctx context.Context, cloud awsup.AWSCloud, request *elb.DescribeLoadBalancersInput, filter func(elbtypes.LoadBalancerDescription) bool) ([]elbtypes.LoadBalancerDescription, error) {
	var found []elbtypes.LoadBalancerDescription
	paginator := elb.NewDescribeLoadBalancersPaginator(cloud.ELB(), request)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("error listing ELBs: %w", err)
		}

		for _, lb := range output.LoadBalancerDescriptions {
			if filter(lb) {
				found = append(found, lb)
			}
		}
	}
	return found, nil
}

func (e *ClassicLoadBalancer) getDNSName() *string {
	return e.DNSName
}

func (e *ClassicLoadBalancer) getHostedZoneId() *string {
	return e.HostedZoneId
}

func (e *ClassicLoadBalancer) Find(c *fi.CloudupContext) (*ClassicLoadBalancer, error) {
	ctx := c.Context()
	cloud := c.T.Cloud.(awsup.AWSCloud)

	lb, err := cloud.FindELBByNameTag(fi.ValueOf(e.Name))
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	actual := &ClassicLoadBalancer{}
	actual.Name = e.Name
	actual.LoadBalancerName = lb.LoadBalancerName
	actual.DNSName = lb.DNSName
	actual.HostedZoneId = lb.CanonicalHostedZoneNameID
	actual.Scheme = lb.Scheme

	// Ignore system fields
	actual.Lifecycle = e.Lifecycle
	actual.WellKnownServices = e.WellKnownServices

	tagMap, err := cloud.DescribeELBTags([]string{*lb.LoadBalancerName})
	if err != nil {
		return nil, err
	}
	actual.Tags = make(map[string]string)
	for _, tag := range tagMap[*e.LoadBalancerName] {
		if strings.HasPrefix(aws.ToString(tag.Key), "aws:cloudformation:") {
			continue
		}
		actual.Tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	for _, subnet := range lb.Subnets {
		actual.Subnets = append(actual.Subnets, &Subnet{ID: aws.String(subnet)})
	}

	for _, sg := range lb.SecurityGroups {
		actual.SecurityGroups = append(actual.SecurityGroups, &SecurityGroup{ID: aws.String(sg)})
	}

	actual.Listeners = make(map[string]*ClassicLoadBalancerListener)

	for _, ld := range lb.ListenerDescriptions {
		l := ld.Listener
		loadBalancerPort := strconv.FormatInt(int64(l.LoadBalancerPort), 10)

		actualListener := &ClassicLoadBalancerListener{}
		actualListener.InstancePort = aws.ToInt32(l.InstancePort)
		actualListener.SSLCertificateID = aws.ToString(l.SSLCertificateId)
		actual.Listeners[loadBalancerPort] = actualListener
	}

	healthcheck, err := findHealthCheck(lb)
	if err != nil {
		return nil, err
	}
	actual.HealthCheck = healthcheck

	// Extract attributes
	lbAttributes, err := findELBAttributes(ctx, cloud, aws.ToString(lb.LoadBalancerName))
	if err != nil {
		return nil, err
	}
	klog.V(4).Infof("ELB attributes: %+v", lbAttributes)

	if lbAttributes != nil {
		actual.AccessLog = &ClassicLoadBalancerAccessLog{
			Enabled: aws.Bool(lbAttributes.AccessLog.Enabled),
		}
		if lbAttributes.AccessLog.EmitInterval != nil {
			actual.AccessLog.EmitInterval = lbAttributes.AccessLog.EmitInterval
		}
		if lbAttributes.AccessLog.S3BucketName != nil {
			actual.AccessLog.S3BucketName = lbAttributes.AccessLog.S3BucketName
		}
		if lbAttributes.AccessLog.S3BucketPrefix != nil {
			actual.AccessLog.S3BucketPrefix = lbAttributes.AccessLog.S3BucketPrefix
		}

		actual.ConnectionDraining = &ClassicLoadBalancerConnectionDraining{}
		if lbAttributes.ConnectionDraining.Enabled {
			actual.ConnectionDraining.Enabled = aws.Bool(lbAttributes.ConnectionDraining.Enabled)
		}
		if lbAttributes.ConnectionDraining.Timeout != nil {
			actual.ConnectionDraining.Timeout = lbAttributes.ConnectionDraining.Timeout
		}

		actual.ConnectionSettings = &ClassicLoadBalancerConnectionSettings{}
		if lbAttributes.ConnectionSettings.IdleTimeout != nil {
			actual.ConnectionSettings.IdleTimeout = lbAttributes.ConnectionSettings.IdleTimeout
		}

		actual.CrossZoneLoadBalancing = &ClassicLoadBalancerCrossZoneLoadBalancing{
			Enabled: aws.Bool(lbAttributes.CrossZoneLoadBalancing.Enabled),
		}
	}

	// Avoid spurious mismatches
	if subnetSlicesEqualIgnoreOrder(actual.Subnets, e.Subnets) {
		actual.Subnets = e.Subnets
	}
	if e.DNSName == nil {
		e.DNSName = actual.DNSName
	}
	if e.HostedZoneId == nil {
		e.HostedZoneId = actual.HostedZoneId
	}
	if e.LoadBalancerName == nil {
		e.LoadBalancerName = actual.LoadBalancerName
	}

	// We allow for the LoadBalancerName to be wrong:
	// 1. We don't want to force a rename of the ELB, because that is a destructive operation
	// 2. We were creating ELBs with insufficiently qualified names previously
	if fi.ValueOf(e.LoadBalancerName) != fi.ValueOf(actual.LoadBalancerName) {
		klog.V(2).Infof("Reusing existing load balancer with name: %q", aws.ToString(actual.LoadBalancerName))
		e.LoadBalancerName = actual.LoadBalancerName
	}

	_ = actual.Normalize(c)

	klog.V(4).Infof("Found ELB %+v", actual)

	return actual, nil
}

var _ fi.HasAddress = &ClassicLoadBalancer{}

// GetWellKnownServices implements fi.HasAddress::GetWellKnownServices.
// It indicates which services we support with this address (likely attached to a load balancer).
func (e *ClassicLoadBalancer) GetWellKnownServices() []wellknownservices.WellKnownService {
	return e.WellKnownServices
}

func (e *ClassicLoadBalancer) FindAddresses(context *fi.CloudupContext) ([]string, error) {
	cloud := context.T.Cloud.(awsup.AWSCloud)

	lb, err := cloud.FindELBByNameTag(fi.ValueOf(e.Name))
	if err != nil {
		return nil, err
	}
	if lb == nil {
		return nil, nil
	}

	lbDnsName := fi.ValueOf(lb.DNSName)
	if lbDnsName == "" {
		return nil, nil
	}
	return []string{lbDnsName}, nil
}

func (e *ClassicLoadBalancer) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(e, c)
}

func (_ *ClassicLoadBalancer) ShouldCreate(a, e, changes *ClassicLoadBalancer) (bool, error) {
	if fi.ValueOf(e.Shared) {
		return false, nil
	}
	return true, nil
}

func (e *ClassicLoadBalancer) Normalize(c *fi.CloudupContext) error {
	// We need to sort our arrays consistently, so we don't get spurious changes
	sort.Stable(OrderSubnetsById(e.Subnets))
	sort.Stable(OrderSecurityGroupsById(e.SecurityGroups))
	return nil
}

func (s *ClassicLoadBalancer) CheckChanges(a, e, changes *ClassicLoadBalancer) error {
	if a == nil {
		if fi.ValueOf(e.Name) == "" {
			return fi.RequiredField("Name")
		}

		shared := fi.ValueOf(e.Shared)
		if !shared {
			if len(e.SecurityGroups) == 0 {
				return fi.RequiredField("SecurityGroups")
			}
			if len(e.Subnets) == 0 {
				return fi.RequiredField("Subnets")
			}
		}

		if e.AccessLog != nil {
			if e.AccessLog.Enabled == nil {
				return fi.RequiredField("Acceslog.Enabled")
			}
			if *e.AccessLog.Enabled {
				if e.AccessLog.S3BucketName == nil {
					return fi.RequiredField("Acceslog.S3Bucket")
				}
			}
		}
		if e.ConnectionDraining != nil {
			if e.ConnectionDraining.Enabled == nil {
				return fi.RequiredField("ConnectionDraining.Enabled")
			}
		}

		if e.CrossZoneLoadBalancing != nil {
			if e.CrossZoneLoadBalancing.Enabled == nil {
				return fi.RequiredField("CrossZoneLoadBalancing.Enabled")
			}
		}
	}

	return nil
}

func (_ *ClassicLoadBalancer) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *ClassicLoadBalancer) error {
	shared := fi.ValueOf(e.Shared)
	if shared {
		return nil
	}
	ctx := context.TODO()

	var loadBalancerName string
	if a == nil {
		if e.LoadBalancerName == nil {
			return fi.RequiredField("LoadBalancerName")
		}
		loadBalancerName = *e.LoadBalancerName

		request := &elb.CreateLoadBalancerInput{}
		request.LoadBalancerName = e.LoadBalancerName
		request.Scheme = e.Scheme

		for _, subnet := range e.Subnets {
			request.Subnets = append(request.Subnets, aws.ToString(subnet.ID))
		}

		for _, sg := range e.SecurityGroups {
			request.SecurityGroups = append(request.SecurityGroups, aws.ToString(sg.ID))
		}

		request.Listeners = []elbtypes.Listener{}

		for loadBalancerPort, listener := range e.Listeners {
			loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 32)
			if err != nil {
				return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
			}
			awsListener := listener.mapToAWS(int32(loadBalancerPortInt))
			request.Listeners = append(request.Listeners, awsListener)
		}

		klog.V(2).Infof("Creating ELB with Name:%q", loadBalancerName)

		response, err := t.Cloud.ELB().CreateLoadBalancer(ctx, request)
		if err != nil {
			return fmt.Errorf("error creating ELB: %v", err)
		}

		e.DNSName = response.DNSName

		// Requery to get the CanonicalHostedZoneNameID
		lb, err := findLoadBalancerByLoadBalancerName(ctx, t.Cloud, loadBalancerName)
		if err != nil {
			return err
		}
		if lb == nil {
			// TODO: Retry?  Is this async
			return fmt.Errorf("Unable to find newly created ELB %q", loadBalancerName)
		}
		e.HostedZoneId = lb.CanonicalHostedZoneNameID
	} else {
		loadBalancerName = fi.ValueOf(a.LoadBalancerName)

		if changes.Subnets != nil {
			var expectedSubnets []string
			for _, s := range e.Subnets {
				expectedSubnets = append(expectedSubnets, fi.ValueOf(s.ID))
			}

			var actualSubnets []string
			for _, s := range a.Subnets {
				actualSubnets = append(actualSubnets, fi.ValueOf(s.ID))
			}

			oldSubnetIDs := slice.GetUniqueStrings(expectedSubnets, actualSubnets)
			if len(oldSubnetIDs) > 0 {
				request := &elb.DetachLoadBalancerFromSubnetsInput{}
				request.LoadBalancerName = aws.String(loadBalancerName)
				request.Subnets = oldSubnetIDs

				klog.V(2).Infof("Detaching Load Balancer from old subnets")
				if _, err := t.Cloud.ELB().DetachLoadBalancerFromSubnets(ctx, request); err != nil {
					return fmt.Errorf("Error detaching Load Balancer from old subnets: %v", err)
				}
			}

			newSubnetIDs := slice.GetUniqueStrings(actualSubnets, expectedSubnets)
			if len(newSubnetIDs) > 0 {
				request := &elb.AttachLoadBalancerToSubnetsInput{}
				request.LoadBalancerName = aws.String(loadBalancerName)
				request.Subnets = newSubnetIDs

				klog.V(2).Infof("Attaching Load Balancer to new subnets")
				if _, err := t.Cloud.ELB().AttachLoadBalancerToSubnets(ctx, request); err != nil {
					return fmt.Errorf("Error attaching Load Balancer to new subnets: %v", err)
				}
			}
		}

		if changes.SecurityGroups != nil {
			request := &elb.ApplySecurityGroupsToLoadBalancerInput{}
			request.LoadBalancerName = aws.String(loadBalancerName)
			for _, sg := range e.SecurityGroups {
				request.SecurityGroups = append(request.SecurityGroups, aws.ToString(sg.ID))
			}

			klog.V(2).Infof("Updating Load Balancer Security Groups")
			if _, err := t.Cloud.ELB().ApplySecurityGroupsToLoadBalancer(ctx, request); err != nil {
				return fmt.Errorf("Error updating security groups on Load Balancer: %v", err)
			}
		}

		if changes.Listeners != nil {

			elbDescription, err := findLoadBalancerByLoadBalancerName(ctx, t.Cloud, loadBalancerName)
			if err != nil {
				return fmt.Errorf("error getting load balancer by name: %v", err)
			}

			if elbDescription != nil {
				// deleting the listener before recreating it
				t.Cloud.ELB().DeleteLoadBalancerListeners(ctx, &elb.DeleteLoadBalancerListenersInput{
					LoadBalancerName:  aws.String(loadBalancerName),
					LoadBalancerPorts: []int32{443},
				})
			}

			request := &elb.CreateLoadBalancerListenersInput{}
			request.LoadBalancerName = aws.String(loadBalancerName)

			for loadBalancerPort, listener := range changes.Listeners {
				loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 32)
				if err != nil {
					return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
				}
				awsListener := listener.mapToAWS(int32(loadBalancerPortInt))
				request.Listeners = append(request.Listeners, awsListener)
			}

			klog.V(2).Infof("Creating LoadBalancer listeners")

			_, err = t.Cloud.ELB().CreateLoadBalancerListeners(ctx, request)
			if err != nil {
				return fmt.Errorf("error creating LoadBalancerListeners: %v", err)
			}
		}
	}

	if err := t.AddELBTags(loadBalancerName, e.Tags); err != nil {
		return err
	}

	if err := t.RemoveELBTags(loadBalancerName, e.Tags); err != nil {
		return err
	}

	if changes.HealthCheck != nil && e.HealthCheck != nil {
		request := &elb.ConfigureHealthCheckInput{}
		request.LoadBalancerName = aws.String(loadBalancerName)
		request.HealthCheck = &elbtypes.HealthCheck{
			Target:             e.HealthCheck.Target,
			HealthyThreshold:   e.HealthCheck.HealthyThreshold,
			UnhealthyThreshold: e.HealthCheck.UnhealthyThreshold,
			Interval:           e.HealthCheck.Interval,
			Timeout:            e.HealthCheck.Timeout,
		}

		klog.V(2).Infof("Configuring health checks on ELB %q", loadBalancerName)

		_, err := t.Cloud.ELB().ConfigureHealthCheck(ctx, request)
		if err != nil {
			return fmt.Errorf("error configuring health checks on ELB: %v", err)
		}
	}

	if err := e.modifyLoadBalancerAttributes(t, a, e, changes); err != nil {
		klog.Infof("error modifying ELB attributes: %v", err)
		return err
	}

	return nil
}

// OrderLoadBalancersByName implements sort.Interface for []OrderLoadBalancersByName, based on name
type OrderLoadBalancersByName []*ClassicLoadBalancer

func (a OrderLoadBalancersByName) Len() int      { return len(a) }
func (a OrderLoadBalancersByName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a OrderLoadBalancersByName) Less(i, j int) bool {
	return fi.ValueOf(a[i].Name) < fi.ValueOf(a[j].Name)
}

type terraformLoadBalancer struct {
	LoadBalancerName *string                          `cty:"name"`
	Listener         []*terraformLoadBalancerListener `cty:"listener"`
	SecurityGroups   []*terraformWriter.Literal       `cty:"security_groups"`
	Subnets          []*terraformWriter.Literal       `cty:"subnets"`
	Internal         *bool                            `cty:"internal"`

	HealthCheck *terraformLoadBalancerHealthCheck `cty:"health_check"`
	AccessLog   *terraformLoadBalancerAccessLog   `cty:"access_logs"`

	ConnectionDraining        *bool  `cty:"connection_draining"`
	ConnectionDrainingTimeout *int32 `cty:"connection_draining_timeout"`

	CrossZoneLoadBalancing *bool `cty:"cross_zone_load_balancing"`

	IdleTimeout *int32 `cty:"idle_timeout"`

	Tags map[string]string `cty:"tags"`
}

type terraformLoadBalancerListener struct {
	InstancePort     int32   `cty:"instance_port"`
	InstanceProtocol string  `cty:"instance_protocol"`
	LBPort           int32   `cty:"lb_port"`
	LBProtocol       string  `cty:"lb_protocol"`
	SSLCertificateID *string `cty:"ssl_certificate_id"`
}

type terraformLoadBalancerHealthCheck struct {
	Target             *string `cty:"target"`
	HealthyThreshold   *int32  `cty:"healthy_threshold"`
	UnhealthyThreshold *int32  `cty:"unhealthy_threshold"`
	Interval           *int32  `cty:"interval"`
	Timeout            *int32  `cty:"timeout"`
}

func (_ *ClassicLoadBalancer) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *ClassicLoadBalancer) error {
	shared := fi.ValueOf(e.Shared)
	if shared {
		return nil
	}

	cloud := t.Cloud.(awsup.AWSCloud)

	if e.LoadBalancerName == nil {
		return fi.RequiredField("LoadBalancerName")
	}

	tf := &terraformLoadBalancer{
		LoadBalancerName: e.LoadBalancerName,
	}
	if fi.ValueOf(e.Scheme) == "internal" {
		tf.Internal = fi.PtrTo(true)
	}

	for _, subnet := range e.Subnets {
		tf.Subnets = append(tf.Subnets, subnet.TerraformLink())
	}
	terraformWriter.SortLiterals(tf.Subnets)

	for _, sg := range e.SecurityGroups {
		tf.SecurityGroups = append(tf.SecurityGroups, sg.TerraformLink())
	}
	terraformWriter.SortLiterals(tf.SecurityGroups)

	for loadBalancerPort, listener := range e.Listeners {
		loadBalancerPortInt, err := strconv.ParseInt(loadBalancerPort, 10, 64)
		if err != nil {
			return fmt.Errorf("error parsing load balancer listener port: %q", loadBalancerPort)
		}

		if listener.SSLCertificateID != "" {
			tf.Listener = append(tf.Listener, &terraformLoadBalancerListener{
				InstanceProtocol: "SSL",
				InstancePort:     listener.InstancePort,
				LBPort:           int32(loadBalancerPortInt),
				LBProtocol:       "SSL",
				SSLCertificateID: &listener.SSLCertificateID,
			})
		} else {
			tf.Listener = append(tf.Listener, &terraformLoadBalancerListener{
				InstanceProtocol: "TCP",
				InstancePort:     listener.InstancePort,
				LBPort:           int32(loadBalancerPortInt),
				LBProtocol:       "TCP",
			})
		}

	}

	if e.HealthCheck != nil {
		tf.HealthCheck = &terraformLoadBalancerHealthCheck{
			Target:             e.HealthCheck.Target,
			HealthyThreshold:   e.HealthCheck.HealthyThreshold,
			UnhealthyThreshold: e.HealthCheck.UnhealthyThreshold,
			Interval:           e.HealthCheck.Interval,
			Timeout:            e.HealthCheck.Timeout,
		}
	}

	if e.AccessLog != nil && fi.ValueOf(e.AccessLog.Enabled) {
		tf.AccessLog = &terraformLoadBalancerAccessLog{
			EmitInterval:   e.AccessLog.EmitInterval,
			Enabled:        e.AccessLog.Enabled,
			S3BucketName:   e.AccessLog.S3BucketName,
			S3BucketPrefix: e.AccessLog.S3BucketPrefix,
		}
	}

	if e.ConnectionDraining != nil {
		tf.ConnectionDraining = e.ConnectionDraining.Enabled
		tf.ConnectionDrainingTimeout = e.ConnectionDraining.Timeout
	}

	if e.ConnectionSettings != nil {
		tf.IdleTimeout = e.ConnectionSettings.IdleTimeout
	}

	if e.CrossZoneLoadBalancing != nil {
		tf.CrossZoneLoadBalancing = e.CrossZoneLoadBalancing.Enabled
	}

	tags := cloud.BuildTags(e.Name)
	for k, v := range e.Tags {
		tags[k] = v
	}
	tf.Tags = tags

	return t.RenderResource("aws_elb", *e.Name, tf)
}

func (e *ClassicLoadBalancer) TerraformLink(params ...string) *terraformWriter.Literal {
	shared := fi.ValueOf(e.Shared)
	if shared {
		if e.LoadBalancerName == nil {
			klog.Fatalf("Name must be set, if LB is shared: %s", e)
		}

		klog.V(4).Infof("reusing existing LB with name %q", *e.LoadBalancerName)
		return terraformWriter.LiteralFromStringValue(*e.LoadBalancerName)
	}

	prop := "id"
	if len(params) > 0 {
		prop = params[0]
	}
	return terraformWriter.LiteralProperty("aws_elb", *e.Name, prop)
}
