package upgraders

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	v1 "github.com/openshift/api/config/v1"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	cvMocks "github.com/openshift/managed-upgrade-operator/pkg/clusterversion/mocks"
	mockDrain "github.com/openshift/managed-upgrade-operator/pkg/drain/mocks"
	dvoMocks "github.com/openshift/managed-upgrade-operator/pkg/dvo/mocks"
	emMocks "github.com/openshift/managed-upgrade-operator/pkg/eventmanager/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	mockMachinery "github.com/openshift/managed-upgrade-operator/pkg/machinery/mocks"
	mockMaintenance "github.com/openshift/managed-upgrade-operator/pkg/maintenance/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	mockMetrics "github.com/openshift/managed-upgrade-operator/pkg/metrics/mocks"
	mockScaler "github.com/openshift/managed-upgrade-operator/pkg/scaler/mocks"
	"github.com/openshift/managed-upgrade-operator/util/mocks"
	testStructs "github.com/openshift/managed-upgrade-operator/util/mocks/structs"
)

var _ = Describe("HealthCheck Step", func() {
	var (
		logger logr.Logger
		// mocks
		mockKubeClient           *mocks.MockClient
		mockCtrl                 *gomock.Controller
		mockMaintClient          *mockMaintenance.MockMaintenance
		mockScalerClient         *mockScaler.MockScaler
		mockMachineryClient      *mockMachinery.MockMachinery
		mockMetricsClient        *mockMetrics.MockMetrics
		mockCVClient             *cvMocks.MockClusterVersion
		mockDrainStrategyBuilder *mockDrain.MockNodeDrainStrategyBuilder
		mockEMClient             *emMocks.MockEventManager
		mockdvoclient            *dvoMocks.MockDvoClient
		mockdvobuilderclient     *dvoMocks.MockDvoClientBuilder

		// upgradeconfig to be used during tests
		upgradeConfigName types.NamespacedName
		upgradeConfig     *upgradev1alpha1.UpgradeConfig

		// upgrader to be used during tests
		config   *upgraderConfig
		upgrader *clusterUpgrader

		mockClusterVersion *v1.ClusterVersion
	)

	BeforeEach(func() {
		upgradeConfigName = types.NamespacedName{
			Name:      "test-upgradeconfig",
			Namespace: "test-namespace",
		}
		upgradeConfig = testStructs.NewUpgradeConfigBuilder().WithNamespacedName(upgradeConfigName).WithPhase(upgradev1alpha1.UpgradePhaseNew).GetUpgradeConfig()
		mockCtrl = gomock.NewController(GinkgoT())
		mockKubeClient = mocks.NewMockClient(mockCtrl)
		mockMaintClient = mockMaintenance.NewMockMaintenance(mockCtrl)
		mockMetricsClient = mockMetrics.NewMockMetrics(mockCtrl)
		mockScalerClient = mockScaler.NewMockScaler(mockCtrl)
		mockMachineryClient = mockMachinery.NewMockMachinery(mockCtrl)
		mockCVClient = cvMocks.NewMockClusterVersion(mockCtrl)
		mockDrainStrategyBuilder = mockDrain.NewMockNodeDrainStrategyBuilder(mockCtrl)
		mockEMClient = emMocks.NewMockEventManager(mockCtrl)
		mockdvoclient = dvoMocks.NewMockDvoClient(mockCtrl)
		mockdvobuilderclient = dvoMocks.NewMockDvoClientBuilder(mockCtrl)
		logger = logf.Log.WithName("cluster upgrader test logger")
		config = buildTestUpgraderConfig(90, 30, 8, 120, 30)
		config.HealthCheck = healthCheck{
			IgnoredCriticals:  []string{"alert1", "alert2"},
			IgnoredNamespaces: []string{"ns1"},
		}
		upgrader = &clusterUpgrader{
			client:               mockKubeClient,
			metrics:              mockMetricsClient,
			cvClient:             mockCVClient,
			notifier:             mockEMClient,
			config:               config,
			scaler:               mockScalerClient,
			drainstrategyBuilder: mockDrainStrategyBuilder,
			maintenance:          mockMaintClient,
			machinery:            mockMachineryClient,
			upgradeConfig:        upgradeConfig,
			dvo:                  mockdvobuilderclient,
		}
		oldTime := time.Now().AddDate(0, 0, -10)
		currentTime := time.Now()
		mockClusterVersion = &v1.ClusterVersion{
			Spec: v1.ClusterVersionSpec{
				DesiredUpdate: &v1.Update{Version: "4.15.4"},
				Channel:       "stable-4.15",
			},
			Status: v1.ClusterVersionStatus{
				History: []v1.UpdateHistory{
					{State: v1.CompletedUpdate, Version: "4.15.1", CompletionTime: &metav1.Time{Time: oldTime}},
					{State: v1.CompletedUpdate, Version: "4.15.3", CompletionTime: &metav1.Time{Time: currentTime}},
				},
			},
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("When the cluster is healthy and featuregate is disabled", func() {
		Context("When no critical alerts are firing", func() {
			var alertsResponse *metrics.AlertResponse

			JustBeforeEach(func() {
				config.FeatureGate = featureGate{}
				alertsResponse = &metrics.AlertResponse{}
			})
			It("will satisfy a pre-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("will have ignored some critical alerts", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).DoAndReturn(
						func(query string) (*metrics.AlertResponse, error) {
							Expect(strings.Contains(query, `alertname!="`+config.HealthCheck.IgnoredCriticals[0]+`"`)).To(BeTrue())
							Expect(strings.Contains(query, `alertname!="`+config.HealthCheck.IgnoredCriticals[1]+`"`)).To(BeTrue())
							return &metrics.AlertResponse{}, nil
						}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(result).To(BeTrue())
			})
			It("will have ignored alerts in specified namespaces", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).DoAndReturn(
						func(query string) (*metrics.AlertResponse, error) {
							Expect(strings.Contains(query, `namespace!="`+config.HealthCheck.IgnoredNamespaces[0]+`"`)).To(BeTrue())
							return &metrics.AlertResponse{}, nil
						}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(result).To(BeTrue())
			})
		})
		Context("When no operators are degraded and featuregate is disabled", func() {
			var alertsResponse *metrics.AlertResponse

			JustBeforeEach(func() {
				config.FeatureGate = featureGate{}
				alertsResponse = &metrics.AlertResponse{}
			})
			It("will satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})

		})
		Context("Get current version failed should not failed the PHC when cluster is healthy ", func() {
			var alertsResponse *metrics.AlertResponse

			JustBeforeEach(func() {
				config.FeatureGate = featureGate{}
				alertsResponse = &metrics.AlertResponse{}
			})
			It("Get clusterversion failed will still satisfy a pre-Upgrade health check", func() {
				var fakeError = fmt.Errorf("fake get cluster version error")
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(nil, fakeError),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("Get current version failed will still satisfy a pre-Upgrade health check", func() {
				var errorClusterVersion = v1.ClusterVersion{
					Spec: v1.ClusterVersionSpec{
						DesiredUpdate: &v1.Update{Version: "4.15.4"},
						Channel:       "stable-4.15",
					},
					Status: v1.ClusterVersionStatus{},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(&errorClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
	})

	Context("When the cluster is unhealthy and featuregate is disabled", func() {
		Context("When critical alerts are firing", func() {
			var alertsResponse *metrics.AlertResponse
			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{
					Data: metrics.AlertData{
						Result: []metrics.AlertResult{
							{Metric: make(map[string]string), Value: nil},
							{Metric: make(map[string]string), Value: nil},
						},
					},
				}
				config.FeatureGate = featureGate{}
			})
			It("will not satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).ToNot(BeNil())
				Expect(result).To(BeFalse())

			})
		})

		Context("When operators are degraded", func() {
			var alertsResponse *metrics.AlertResponse

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				config.FeatureGate = featureGate{}
			})
			It("will not satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).ToNot(BeNil())
				Expect(result).To(BeFalse())
			})
		})

	})

	Context("When the cluster healthy and featuregate is enabled", func() {
		nodes := &corev1.NodeList{
			TypeMeta: metav1.TypeMeta{},
			ListMeta: metav1.ListMeta{},
			Items: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
				},
			},
		}
		var cordonAddedTime *metav1.Time
		Context("When no critical alerts are firing", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				upgradeConfig.Spec.CapacityReservation = true
			})
			It("will satisfy a pre-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("will have ignored some critical alerts", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).DoAndReturn(
						func(query string) (*metrics.AlertResponse, error) {
							Expect(strings.Contains(query, `alertname!="`+config.HealthCheck.IgnoredCriticals[0]+`"`)).To(BeTrue())
							Expect(strings.Contains(query, `alertname!="`+config.HealthCheck.IgnoredCriticals[1]+`"`)).To(BeTrue())
							return &metrics.AlertResponse{}, nil
						}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(result).To(BeTrue())
			})
			It("will have ignored alerts in specified namespaces", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).DoAndReturn(
						func(query string) (*metrics.AlertResponse, error) {
							Expect(strings.Contains(query, `namespace!="`+config.HealthCheck.IgnoredNamespaces[0]+`"`)).To(BeTrue())
							return &metrics.AlertResponse{}, nil
						}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(result).To(BeTrue())
			})
			It("will satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
		Context("When no operators are degraded", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				upgradeConfig.Spec.CapacityReservation = true
			})

			It("will satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("will satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
		Context("When no node is manually cordoned", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
			})

			It("will satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("will satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
		Context("When node is cordoned by upgrade", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				pdb = &policyv1.PodDisruptionBudgetList{}
			})

			It("will satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
			It("will satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
	})

	Context("When the cluster is unhealthy", func() {
		pdb := &policyv1.PodDisruptionBudgetList{
			TypeMeta: metav1.TypeMeta{},
			ListMeta: metav1.ListMeta{},
			Items: []policyv1.PodDisruptionBudget{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "testPDB"},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
					},
				},
			},
		}
		nodes := &corev1.NodeList{
			TypeMeta: metav1.TypeMeta{},
			ListMeta: metav1.ListMeta{},
			Items: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
				},
			},
		}
		var cordonAddedTime *metav1.Time
		Context("When critical alerts are firing", func() {
			var alertsResponse *metrics.AlertResponse
			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{
					Data: metrics.AlertData{
						Result: []metrics.AlertResult{
							{Metric: make(map[string]string), Value: nil},
							{Metric: make(map[string]string), Value: nil},
						},
					},
				}
			})
			It("will not satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
			It("will not satisfy a pre-Upgrade health check in the upgrade phase", func() {
				upgradeConfig = testStructs.NewUpgradeConfigBuilder().WithNamespacedName(upgradeConfigName).WithPhase(upgradev1alpha1.UpgradePhaseUpgrading).GetUpgradeConfig()
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
			It("will not satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeFalse())
			})
		})

		Context("When operators are degraded", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
			})
			It("will not satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
			It("will not satisfy a post-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
				)
				result, err := upgrader.PostUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeFalse())
			})
		})

		Context("When cluster is not allowed to scale", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}
			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				upgradeConfig.Spec.CapacityReservation = true
			})

			It("will satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(false, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
			})
		})

		Context("When node is cordoned manually", func() {
			var alertsResponse *metrics.AlertResponse
			nodes := &corev1.NodeList{
				TypeMeta: metav1.TypeMeta{},
				ListMeta: metav1.ListMeta{},
				Items: []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
					},
				},
			}
			pdb := &policyv1.PodDisruptionBudgetList{}
			var cordonAddedTime *metav1.Time
			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
			})
			It("will not satisfy a pre-Upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: true, AddedAt: cordonAddedTime}),
					mockMachineryClient.EXPECT().IsNodeUpgrading(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
		})

		Context("When node unschedulable taint check failed", func() {
			var alertsResponse *metrics.AlertResponse
			var addedTime *metav1.Time
			pdb := &policyv1.PodDisruptionBudgetList{}
			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
			})
			It("Memory pressure taint will not satisfy a pre-Upgrade health check", func() {
				nodes := &corev1.NodeList{
					TypeMeta: metav1.TypeMeta{},
					ListMeta: metav1.ListMeta{},
					Items: []corev1.Node{
						{
							ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodeMemoryPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
					},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(true),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})

			It("Disk pressure taint will not satisfy a pre-Upgrade health check", func() {
				nodes := &corev1.NodeList{
					TypeMeta: metav1.TypeMeta{},
					ListMeta: metav1.ListMeta{},
					Items: []corev1.Node{
						{
							ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodeDiskPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
					},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(true),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})

			It("PID pressure taint will not satisfy a pre-Upgrade health check", func() {
				nodes := &corev1.NodeList{
					TypeMeta: metav1.TypeMeta{},
					ListMeta: metav1.ListMeta{},
					Items: []corev1.Node{
						{
							ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodePIDPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
					},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(true),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})

			It("There are 2 pressure taints which will not satisfy a pre-Upgrade health check", func() {
				nodes := &corev1.NodeList{
					TypeMeta: metav1.TypeMeta{},
					ListMeta: metav1.ListMeta{},
					Items: []corev1.Node{
						{
							ObjectMeta: metav1.ObjectMeta{Name: "testNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodeMemoryPressure,
										TimeAdded: addedTime,
									},
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodePIDPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
					},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(true),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(true),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})

			It("There are all 3 pressure taints which will not satisfy a pre-Upgrade health check", func() {
				nodes := &corev1.NodeList{
					TypeMeta: metav1.TypeMeta{},
					ListMeta: metav1.ListMeta{},
					Items: []corev1.Node{
						{
							ObjectMeta: metav1.ObjectMeta{Name: "memPressureNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodeMemoryPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
						{
							ObjectMeta: metav1.ObjectMeta{Name: "diskAndPIDPressureNode"},
							Spec: corev1.NodeSpec{
								Taints: []corev1.Taint{
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodeDiskPressure,
										TimeAdded: addedTime,
									},
									{
										Effect:    corev1.TaintEffectNoSchedule,
										Key:       corev1.TaintNodePIDPressure,
										TimeAdded: addedTime,
									},
								},
							},
						},
					},
				}
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{"ClusterOperator"}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.DefaultWorkerMachinepoolNotFound, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: addedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(true),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(true),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(true),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
		})

		Context("When the cluster's upgrade process has commenced", func() {
			It("will not re-perform a pre-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})

		Context("When the upgrader can't tell if the cluster's upgrade has commenced", func() {
			var fakeError = fmt.Errorf("fake upgradeCommenced error")
			It("will abort the pre-upgrade health check", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, fakeError),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(HaveOccurred())
				Expect(err).To(Equal(fakeError))
				Expect(result).To(BeFalse())
			})
		})
		Context("Test CapacityReservation", func() {
			var alertsResponse *metrics.AlertResponse
			pdb := &policyv1.PodDisruptionBudgetList{}

			JustBeforeEach(func() {
				alertsResponse = &metrics.AlertResponse{}
				upgradeConfig.Spec.CapacityReservation = true
			})
			It("Will not satisfy PHC", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().GetClusterVersion().Return(mockClusterVersion, nil),
					mockMetricsClient.EXPECT().Query(gomock.Any()).Return(alertsResponse, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.MetricsQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.CriticalAlertsFiring, gomock.Any(), gomock.Any()),
					mockCVClient.EXPECT().HasDegradedOperators().Return(&clusterversion.HasDegradedOperatorsResult{Degraded: []string{}}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsStatusFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterOperatorsDegraded, gomock.Any(), gomock.Any()),
					mockScalerClient.EXPECT().CanScale(gomock.Any(), logger).Return(false, fmt.Errorf("CapacityReservationHealthcheckFailed")),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckFailed(upgradeConfig.Name, gomock.Any(), gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().IsNodeCordoned(gomock.Any()).Return(&machinery.IsCordonedResult{IsCordoned: false, AddedAt: cordonAddedTime}),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesManuallyCordoned, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *nodes),
					mockMachineryClient.EXPECT().HasMemoryPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasDiskPressure(gomock.Any()).Return(false),
					mockMachineryClient.EXPECT().HasPidPressure(gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodeQueryFailed, gomock.Any(), gomock.Any()),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterNodesTaintedUnschedulable, gomock.Any(), gomock.Any()),
					mockKubeClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).SetArg(1, *pdb),
					mockdvobuilderclient.EXPECT().New(gomock.Any()).Return(mockdvoclient, nil),
					mockdvoclient.EXPECT().GetMetrics().Return([]byte{}, nil),
					mockMetricsClient.EXPECT().UpdateMetricHealthcheckSucceeded(upgradeConfig.Name, metrics.ClusterInvalidPDB, gomock.Any(), gomock.Any()),
					mockEMClient.EXPECT().NotifyResult(gomock.Any(), gomock.Any()).Return(nil),
				)
				result, err := upgrader.PreUpgradeHealthCheck(context.TODO(), logger)
				Expect(err).To(BeNil())
				Expect(result).To(BeFalse())
			})
		})

	})
})
