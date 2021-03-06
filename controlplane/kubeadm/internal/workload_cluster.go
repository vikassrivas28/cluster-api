/*
Copyright 2020 The Kubernetes Authors.

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

package internal

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"github.com/blang/semver"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/etcd"
	etcdutil "sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/etcd/util"
	"sigs.k8s.io/cluster-api/util/certs"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type etcdClientFor interface {
	forNode(name string) (*etcd.Client, error)
}

// Cluster are operations on workload clusters.
type Cluster struct {
	Client              ctrlclient.Client
	EtcdClientGenerator etcdClientFor
}

func (c *Cluster) getControlPlaneNodes(ctx context.Context) (*corev1.NodeList, error) {
	nodes := &corev1.NodeList{}
	labels := map[string]string{
		"node-role.kubernetes.io/master": "",
	}

	if err := c.Client.List(ctx, nodes, ctrlclient.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (c *Cluster) getConfigMap(ctx context.Context, configMap types.NamespacedName) (*corev1.ConfigMap, error) {
	original := &corev1.ConfigMap{}
	if err := c.Client.Get(ctx, configMap, original); err != nil {
		return nil, errors.Wrapf(err, "error getting %s/%s configmap from target cluster", configMap.Namespace, configMap.Name)
	}
	return original.DeepCopy(), nil
}

// healthCheckResult maps nodes that are checked to any errors the node has related to the check.
type healthCheckResult map[string]error

// controlPlaneIsHealthy does a best effort check of the control plane components the kubeadm control plane cares about.
// The return map is a map of node names as keys to error that that node encountered.
// All nodes will exist in the map with nil errors if there were no errors for that node.
func (c *Cluster) controlPlaneIsHealthy(ctx context.Context) (healthCheckResult, error) {
	controlPlaneNodes, err := c.getControlPlaneNodes(ctx)
	if err != nil {
		return nil, err
	}

	response := make(map[string]error)
	for _, node := range controlPlaneNodes.Items {
		name := node.Name
		response[name] = nil
		apiServerPodKey := types.NamespacedName{
			Namespace: metav1.NamespaceSystem,
			Name:      staticPodName("kube-apiserver", name),
		}
		apiServerPod := &corev1.Pod{}
		if err := c.Client.Get(ctx, apiServerPodKey, apiServerPod); err != nil {
			response[name] = err
			continue
		}
		response[name] = checkStaticPodReadyCondition(apiServerPod)

		controllerManagerPodKey := types.NamespacedName{
			Namespace: metav1.NamespaceSystem,
			Name:      staticPodName("kube-controller-manager", name),
		}
		controllerManagerPod := &corev1.Pod{}
		if err := c.Client.Get(ctx, controllerManagerPodKey, controllerManagerPod); err != nil {
			response[name] = err
			continue
		}
		response[name] = checkStaticPodReadyCondition(controllerManagerPod)
	}

	return response, nil
}

// removeMemberForNode removes etcd member (nodeToRemove) through another (nodeForEtcdClient).
// It's create etcd connection using nodeForEtcdClient to removing nodeToRemove.
func (c *Cluster) removeMemberForNode(ctx context.Context, nodeForEtcdClient, nodeToRemove string) error {
	etcdClient, err := c.EtcdClientGenerator.forNode(nodeForEtcdClient)
	if err != nil {
		return errors.Wrap(err, "failed to create etcd Client")
	}

	// List etcd members. This checks that the member is healthy, because the request goes through consensus.
	members, err := etcdClient.Members(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to list etcd members using etcd Client")
	}
	member := etcdutil.MemberForName(members, nodeToRemove)

	// The member has already been removed, return immediately
	if member == nil {
		return nil
	}

	if err := etcdClient.RemoveMember(ctx, member.ID); err != nil {
		return errors.Wrap(err, "failed to remove member from etcd")
	}

	return nil
}

// etcdIsHealthy runs checks for every etcd member in the cluster to satisfy our definition of healthy.
// This is a best effort check and nodes can become unhealthy after the check is complete. It is not a guarantee.
// It's used a signal for if we should allow a target cluster to scale up, scale down or upgrade.
// It returns a map of nodes checked along with an error for a given node.
func (c *Cluster) etcdIsHealthy(ctx context.Context) (healthCheckResult, error) {
	var knownClusterID uint64
	var knownMemberIDSet etcdutil.UInt64Set

	controlPlaneNodes, err := c.getControlPlaneNodes(ctx)
	if err != nil {
		return nil, err
	}

	response := make(map[string]error)
	for _, node := range controlPlaneNodes.Items {
		name := node.Name
		response[name] = nil
		if node.Spec.ProviderID == "" {
			response[name] = errors.New("empty provider ID")
			continue
		}

		// Create the etcd Client for the etcd Pod scheduled on the Node
		etcdClient, err := c.EtcdClientGenerator.forNode(name)
		if err != nil {
			response[name] = errors.Wrap(err, "failed to create etcd Client")
			continue
		}

		// List etcd members. This checks that the member is healthy, because the request goes through consensus.
		members, err := etcdClient.Members(ctx)
		if err != nil {
			response[name] = errors.Wrap(err, "failed to list etcd members using etcd Client")
			continue
		}
		member := etcdutil.MemberForName(members, name)

		// Check that the member reports no alarms.
		if len(member.Alarms) > 0 {
			response[name] = errors.Errorf("etcd member reports alarms: %v", member.Alarms)
			continue
		}

		// Check that the member belongs to the same cluster as all other members.
		clusterID := member.ClusterID
		if knownClusterID == 0 {
			knownClusterID = clusterID
		} else if knownClusterID != clusterID {
			response[name] = errors.Errorf("etcd member has cluster ID %d, but all previously seen etcd members have cluster ID %d", clusterID, knownClusterID)
			continue
		}

		// Check that the member list is stable.
		memberIDSet := etcdutil.MemberIDSet(members)
		if knownMemberIDSet.Len() == 0 {
			knownMemberIDSet = memberIDSet
		} else {
			unknownMembers := memberIDSet.Difference(knownMemberIDSet)
			if unknownMembers.Len() > 0 {
				response[name] = errors.Errorf("etcd member reports members IDs %v, but all previously seen etcd members reported member IDs %v", memberIDSet.UnsortedList(), knownMemberIDSet.UnsortedList())
			}
			continue
		}
	}

	// Check that there is exactly one etcd member for every control plane machine.
	// There should be no etcd members added "out of band.""
	if len(controlPlaneNodes.Items) != len(knownMemberIDSet) {
		return response, errors.Errorf("there are %d control plane nodes, but %d etcd members", len(controlPlaneNodes.Items), len(knownMemberIDSet))
	}

	return response, nil
}

// UpdateKubernetesVersionInKubeadmConfigMap updates the kubernetes version in the kubeadm config map.
func (c *Cluster) UpdateKubernetesVersionInKubeadmConfigMap(ctx context.Context, version string) error {
	configMapKey := types.NamespacedName{Name: "kubeadm-config", Namespace: metav1.NamespaceSystem}
	kubeadmConfigMap, err := c.getConfigMap(ctx, configMapKey)
	if err != nil {
		return err
	}
	config := &kubeadmConfig{ConfigMap: kubeadmConfigMap}
	if err := config.UpdateKubernetesVersion(version); err != nil {
		return err
	}
	if err := c.Client.Update(ctx, config.ConfigMap); err != nil {
		return errors.Wrap(err, "error updating kubeadm ConfigMap")
	}
	return nil
}

// UpdateKubeletConfigMap will create a new kubelet-config-1.x config map for a new version of the kubelet.
// This is a necessary process for upgrades.
func (c *Cluster) UpdateKubeletConfigMap(ctx context.Context, version semver.Version) error {
	// Check if the desired configmap already exists
	desiredKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor)
	configMapKey := types.NamespacedName{Name: desiredKubeletConfigMapName, Namespace: metav1.NamespaceSystem}
	_, err := c.getConfigMap(ctx, configMapKey)
	if err == nil {
		// Nothing to do, the configmap already exists
		return nil
	}
	if !apierrors.IsNotFound(errors.Cause(err)) {
		return errors.Wrapf(err, "error determining if kubelet configmap %s exists", desiredKubeletConfigMapName)
	}

	previousMinorVersionKubeletConfigMapName := fmt.Sprintf("kubelet-config-%d.%d", version.Major, version.Minor-1)
	configMapKey = types.NamespacedName{Name: previousMinorVersionKubeletConfigMapName, Namespace: metav1.NamespaceSystem}
	// Returns a copy
	cm, err := c.getConfigMap(ctx, configMapKey)
	if apierrors.IsNotFound(errors.Cause(err)) {
		return errors.Errorf("unable to find kubelet configmap %s", previousMinorVersionKubeletConfigMapName)
	}
	if err != nil {
		return err
	}

	// Update the name to the new name
	cm.Name = desiredKubeletConfigMapName
	// Clear the resource version. Is this necessary since this cm is actually a DeepCopy()?
	cm.ResourceVersion = ""

	if err := c.Client.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "error creating configmap %s", desiredKubeletConfigMapName)
	}
	return nil
}

// RemoveEtcdMemberForMachine removes the etcd member from the target cluster's etcd cluster.
func (c *Cluster) RemoveEtcdMemberForMachine(ctx context.Context, machine *clusterv1.Machine) error {
	if machine == nil || machine.Status.NodeRef == nil {
		// Nothing to do, no node for Machine
		return nil
	}

	controlPlaneNodes, err := c.getControlPlaneNodes(ctx)
	if err != nil {
		return err
	}

	nodeToRemove := machine.Status.NodeRef.Name
	errs := []error{}

	// Try all node other than nodeToRemove for proxying etcd Client.
	// and returns the first successful response.
	for _, node := range controlPlaneNodes.Items {
		nodeForEtcdClient := node.Name
		if nodeForEtcdClient == nodeToRemove {
			continue
		}

		err = c.removeMemberForNode(ctx, nodeForEtcdClient, nodeToRemove)
		if err == nil {
			return nil
		}

		errs = append(errs, err)
	}

	return kerrors.NewAggregate(errs)
}

// RemoveMachineFromKubeadmConfigMap removes the entry for the machine from the kubeadm configmap.
func (c *Cluster) RemoveMachineFromKubeadmConfigMap(ctx context.Context, machine *clusterv1.Machine) error {
	if machine == nil || machine.Status.NodeRef == nil {
		// Nothing to do, no node for Machine
		return nil
	}

	configMapKey := types.NamespacedName{Name: "kubeadm-config", Namespace: metav1.NamespaceSystem}
	kubeadmConfigMap, err := c.getConfigMap(ctx, configMapKey)
	if err != nil {
		return err
	}
	config := &kubeadmConfig{ConfigMap: kubeadmConfigMap}
	if err := config.RemoveAPIEndpoint(machine.Status.NodeRef.Name); err != nil {
		return err
	}
	if err := c.Client.Update(ctx, config.ConfigMap); err != nil {
		return errors.Wrap(err, "error updating kubeadm ConfigMap")
	}
	return nil
}

func generateClientCert(caCertEncoded, caKeyEncoded []byte) (tls.Certificate, error) {
	privKey, err := certs.NewPrivateKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	caCert, err := certs.DecodeCertPEM(caCertEncoded)
	if err != nil {
		return tls.Certificate{}, err
	}
	caKey, err := certs.DecodePrivateKeyPEM(caKeyEncoded)
	if err != nil {
		return tls.Certificate{}, err
	}
	x509Cert, err := newClientCert(caCert, privKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certs.EncodeCertPEM(x509Cert), certs.EncodePrivateKeyPEM(privKey))
}

func newClientCert(caCert *x509.Certificate, key *rsa.PrivateKey, caKey *rsa.PrivateKey) (*x509.Certificate, error) {
	cfg := certs.Config{
		CommonName: "cluster-api.x-k8s.io",
	}

	now := time.Now().UTC()

	tmpl := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		NotBefore:   now.Add(time.Minute * -5),
		NotAfter:    now.Add(time.Hour * 24 * 365 * 10), // 10 years
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	b, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create signed Client certificate: %+v", tmpl)
	}

	c, err := x509.ParseCertificate(b)
	return c, errors.WithStack(err)
}

func staticPodName(component, nodeName string) string {
	return fmt.Sprintf("%s-%s", component, nodeName)
}

func checkStaticPodReadyCondition(pod *corev1.Pod) error {
	found := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			found = true
		}
		if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
			return errors.Errorf("static pod %s/%s is not ready", pod.Namespace, pod.Name)
		}
	}
	if !found {
		return errors.Errorf("pod does not have ready condition: %v", pod.Name)
	}
	return nil
}
