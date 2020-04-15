package target

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	fleet "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	"github.com/rancher/fleet/pkg/bundle"
	fleetcontrollers "github.com/rancher/fleet/pkg/generated/controllers/fleet.cattle.io/v1alpha1"
	"github.com/rancher/fleet/pkg/manifest"
	"github.com/rancher/fleet/pkg/options"
	"github.com/rancher/fleet/pkg/summary"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	cgByNamespace = "ClusterGroupByNamespace"
)

var (
	defLimit                    = intstr.FromString("10%")
	defAutoPartitionSize        = intstr.FromString("25%")
	defMaxUnavailablePartitions = intstr.FromInt(0)
)

type Manager struct {
	clusters              fleetcontrollers.ClusterCache
	clusterGroups         fleetcontrollers.ClusterGroupCache
	bundleDeploymentCache fleetcontrollers.BundleDeploymentCache
	bundleCache           fleetcontrollers.BundleCache
	contentStore          manifest.Store
}

func New(
	clusters fleetcontrollers.ClusterCache,
	clusterGroups fleetcontrollers.ClusterGroupCache,
	bundles fleetcontrollers.BundleCache,
	contentStore manifest.Store,
	bundleDeployments fleetcontrollers.BundleDeploymentCache) *Manager {

	clusterGroups.AddIndexer(cgByNamespace, func(obj *fleet.ClusterGroup) ([]string, error) {
		if obj.Status.Namespace == "" {
			return nil, nil
		}
		return []string{obj.Status.Namespace}, nil
	})

	return &Manager{
		clusterGroups:         clusterGroups,
		clusters:              clusters,
		bundleDeploymentCache: bundleDeployments,
		bundleCache:           bundles,
		contentStore:          contentStore,
	}
}

func (m *Manager) ClusterGroup(cluster *fleet.Cluster) (*fleet.ClusterGroup, error) {
	cgs, err := m.clusterGroups.GetByIndex(cgByNamespace, cluster.Namespace)
	if err != nil {
		return nil, err
	}
	if len(cgs) > 0 {
		return cgs[0], nil
	}
	return nil, nil
}

func (m *Manager) BundleForDeployment(bd *fleet.BundleDeployment) (string, string) {
	return bd.Labels["fleet.cattle.io/bundle-deployment-namespace"],
		bd.Labels["fleet.cattle.io/bundle-deployment-name"]
}

func (m *Manager) BundlesForCluster(cluster *fleet.Cluster) (result []*fleet.Bundle, _ error) {
	cg, err := m.ClusterGroup(cluster)
	if err != nil || cg == nil {
		return nil, err
	}

	bundles, err := m.bundleCache.List(cg.Namespace, labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, app := range bundles {
		bundle, err := bundle.New(app)
		if err != nil {
			logrus.Errorf("ignore bad app %s/%s: %v", app.Namespace, app.Name, err)
			continue
		}

		m := bundle.Match(cg.Name, cg.Labels, cluster.Labels)
		if m != nil {
			result = append(result, app)
		}
	}

	return
}

func (m *Manager) Targets(fleetBundle *fleet.Bundle) (result []*Target, _ error) {
	bundle, err := bundle.New(fleetBundle)
	if err != nil {
		return nil, err
	}

	clusters, err := m.clusters.List("", labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		cg, err := m.ClusterGroup(cluster)
		if err != nil {
			return nil, err
		}
		if cg == nil || cg.Namespace != fleetBundle.Namespace {
			continue
		}

		match := bundle.Match(cg.Name, cg.Labels, cluster.Labels)
		if match == nil {
			continue
		}

		manifest, err := match.Manifest()
		if err != nil {
			return nil, err
		}

		opts, err := options.Calculate(&fleetBundle.Spec, match.Target)
		if err != nil {
			return nil, err
		}

		deploymentID, err := options.DeploymentID(manifest, opts)
		if err != nil {
			return nil, err
		}

		if _, err := m.contentStore.Store(manifest); err != nil {
			return nil, err
		}

		result = append(result, &Target{
			ClusterGroup: cg,
			Cluster:      cluster,
			Target:       match.Target,
			Bundle:       fleetBundle,
			Options:      opts,
			DeploymentID: deploymentID,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Cluster.Name < result[j].Cluster.Name
	})

	return result, m.foldInDeployments(fleetBundle, result)
}

func (m *Manager) foldInDeployments(app *fleet.Bundle, targets []*Target) error {
	bundleDeployments, err := m.bundleDeploymentCache.List("", labels.SelectorFromSet(DeploymentLabels(app)))
	if err != nil {
		return err
	}

	byNamespace := map[string]*fleet.BundleDeployment{}
	for _, appDep := range bundleDeployments {
		byNamespace[appDep.Namespace] = appDep.DeepCopy()
	}

	for _, target := range targets {
		target.Deployment = byNamespace[target.Cluster.Status.Namespace]
	}

	return nil
}

func DeploymentLabels(app *fleet.Bundle) map[string]string {
	return map[string]string{
		"fleet.cattle.io/bundle-deployment-name":      app.Name,
		"fleet.cattle.io/bundle-deployment-namespace": app.Namespace,
	}
}

type Target struct {
	Deployment   *fleet.BundleDeployment
	ClusterGroup *fleet.ClusterGroup
	Cluster      *fleet.Cluster
	Bundle       *fleet.Bundle
	Target       *fleet.BundleTarget
	Options      fleet.BundleDeploymentOptions
	DeploymentID string
}

func (t *Target) IsPaused() bool {
	return t.Cluster.Spec.Paused ||
		t.ClusterGroup.Spec.Pause ||
		t.Bundle.Spec.Paused
}

func (t *Target) AssignNewDeployment() {
	t.Deployment = &fleet.BundleDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.Bundle.Name,
			Namespace: t.Cluster.Status.Namespace,
			Labels:    DeploymentLabels(t.Bundle),
		},
	}
}

func getRollout(targets []*Target) *fleet.RolloutStrategy {
	var rollout *fleet.RolloutStrategy
	if len(targets) > 0 {
		rollout = targets[0].Bundle.Spec.RolloutStrategy
	}
	if rollout == nil {
		rollout = &fleet.RolloutStrategy{}
	}
	return rollout
}

func Limit(count int, val ...*intstr.IntOrString) (int, error) {
	if count == 0 {
		return 1, nil
	}

	var maxUnavailable *intstr.IntOrString

	for _, val := range val {
		if val != nil {
			maxUnavailable = val
			break
		}
	}

	if maxUnavailable == nil {
		maxUnavailable = &defLimit
	}

	if maxUnavailable.Type == intstr.Int {
		return maxUnavailable.IntValue(), nil
	}

	i := maxUnavailable.IntValue()
	if i > 0 {
		return i, nil
	}

	if !strings.HasSuffix(maxUnavailable.StrVal, "%") {
		return 0, fmt.Errorf("invalid maxUnavailable, must be int or percentage (ending with %%): %s", maxUnavailable)
	}

	percent, err := strconv.ParseFloat(strings.TrimSuffix(maxUnavailable.StrVal, "%"), 64)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse %s", maxUnavailable.StrVal)
	}

	if percent <= 0 {
		return 1, nil
	}

	i = int(float64(count)*percent) / 100
	if i <= 0 {
		return 1, nil
	}

	return i, nil
}

func MaxUnavailable(targets []*Target) (int, error) {
	rollout := getRollout(targets)
	return Limit(len(targets), rollout.MaxUnavailable)
}

func MaxUnavailablePartitions(partitions []Partition, targets []*Target) (int, error) {
	rollout := getRollout(targets)
	return Limit(len(partitions), rollout.MaxUnavailablePartitions, &defMaxUnavailablePartitions)
}

func IsPartitionUnavailable(status *fleet.PartitionStatus, targets []*Target) bool {
	// Unavailable for a partition is stricter than unavailable for a target.
	// For a partition a target must be available and update to date.
	status.Unavailable = 0
	for _, target := range targets {
		if !UpToDate(target) || IsUnavailable(target.Deployment) {
			status.Unavailable++
		}
	}

	return status.Unavailable > status.MaxUnavailable
}

func UpToDate(target *Target) bool {
	if target.Deployment == nil ||
		target.Deployment.Spec.StagedDeploymentID != target.DeploymentID ||
		target.Deployment.Spec.DeploymentID != target.DeploymentID ||
		target.Deployment.Status.AppliedDeploymentID != target.DeploymentID {
		return false
	}

	return true
}

func Unavailable(targets []*Target) (count int) {
	for _, target := range targets {
		if target.Deployment == nil {
			continue
		}
		if IsUnavailable(target.Deployment) {
			count++
		}
	}
	return
}

func IsUnavailable(target *fleet.BundleDeployment) bool {
	if target == nil {
		return false
	}
	return target.Status.AppliedDeploymentID != target.Spec.DeploymentID ||
		!target.Status.Ready
}

func (t *Target) State() fleet.BundleState {
	switch {
	case t.Deployment == nil:
		return fleet.Pending
	default:
		return summary.GetDeploymentState(t.Deployment)
	}
}

func (t *Target) Message() string {
	return summary.MessageFromDeployment(t.Deployment)
}

func Summary(targets []*Target) fleet.BundleSummary {
	var bundleSummary fleet.BundleSummary
	for _, currentTarget := range targets {
		cluster := currentTarget.Cluster.Namespace + "/" + currentTarget.Cluster.Name
		summary.IncrementState(&bundleSummary, cluster, currentTarget.State(), currentTarget.Message())
		bundleSummary.DesiredReady++
	}
	return bundleSummary
}
