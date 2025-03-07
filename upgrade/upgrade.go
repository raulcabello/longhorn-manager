package upgrade

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/longhorn/longhorn-manager/types"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	lhclientset "github.com/longhorn/longhorn-manager/k8s/pkg/client/clientset/versioned"

	"github.com/longhorn/longhorn-manager/upgrade/v070to080"
	"github.com/longhorn/longhorn-manager/upgrade/v100to101"
	"github.com/longhorn/longhorn-manager/upgrade/v102to110"
	"github.com/longhorn/longhorn-manager/upgrade/v110to111"
	"github.com/longhorn/longhorn-manager/upgrade/v110to120"
	"github.com/longhorn/longhorn-manager/upgrade/v111to120"
	"github.com/longhorn/longhorn-manager/upgrade/v120to121"
	"github.com/longhorn/longhorn-manager/upgrade/v122to123"
	"github.com/longhorn/longhorn-manager/upgrade/v1beta1"
)

const (
	LeaseLockName = "longhorn-manager-upgrade-lock"
)

func Upgrade(kubeconfigPath, currentNodeID string) error {
	namespace := os.Getenv(types.EnvPodNamespace)
	if namespace == "" {
		logrus.Warnf("Cannot detect pod namespace, environment variable %v is missing, "+
			"using default namespace", types.EnvPodNamespace)
		namespace = corev1.NamespaceDefault
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return errors.Wrap(err, "unable to get client config")
	}

	kubeClient, err := clientset.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	lhClient, err := lhclientset.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "unable to get clientset")
	}

	scheme := runtime.NewScheme()
	if err := longhorn.SchemeBuilder.AddToScheme(scheme); err != nil {
		return errors.Wrap(err, "unable to create scheme")
	}

	if err := upgradeLocalNode(); err != nil {
		return err
	}

	if err := upgrade(currentNodeID, namespace, config, lhClient, kubeClient); err != nil {
		return err
	}

	return nil
}

func upgrade(currentNodeID, namespace string, config *restclient.Config, lhClient *lhclientset.Clientset, kubeClient *clientset.Clientset) error {
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	defer cancel()

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      LeaseLockName,
			Namespace: namespace,
		},
		Client: kubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: currentNodeID,
		},
	}

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   20 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				defer cancel()
				defer func() {
					if err != nil {
						logrus.Errorf("Upgrade failed: %v", err)
					} else {
						logrus.Infof("Finish upgrading")
					}
				}()
				logrus.Infof("Start upgrading")
				if err = doAPIVersionUpgrade(namespace, config, lhClient); err != nil {
					return
				}
				if err = doCRUpgrade(namespace, lhClient, kubeClient); err != nil {
					return
				}
				if err = doPodsUpgrade(namespace, lhClient, kubeClient); err != nil {
					return
				}
				if err = doServicesUpgrade(namespace, kubeClient); err != nil {
					return
				}
				if err = doDeploymentAndDaemonSetUpgrade(namespace, kubeClient); err != nil {
					return
				}
			},
			OnStoppedLeading: func() {
				logrus.Infof("Upgrade leader lost: %s", currentNodeID)
			},
			OnNewLeader: func(identity string) {
				if identity == currentNodeID {
					return
				}
				logrus.Infof("New upgrade leader elected: %s", identity)
			},
		},
	})

	return err
}

func doAPIVersionUpgrade(namespace string, config *restclient.Config, lhClient *lhclientset.Clientset) (err error) {
	defer func() {
		err = errors.Wrap(err, "upgrade API version failed")
	}()

	crdAPIVersion := ""

	crdAPIVersionSetting, err := lhClient.LonghornV1beta2().Settings(namespace).Get(context.TODO(), string(types.SettingNameCRDAPIVersion), metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else {
		crdAPIVersion = crdAPIVersionSetting.Value
	}

	if crdAPIVersion != "" &&
		crdAPIVersion != types.CRDAPIVersionV1beta1 &&
		crdAPIVersion != types.CRDAPIVersionV1beta2 {
		return fmt.Errorf("unrecognized CRD API version %v", crdAPIVersion)
	}

	if crdAPIVersion == types.CurrentCRDAPIVersion {
		logrus.Info("No API version upgrade is needed")
		return nil
	}

	switch crdAPIVersion {
	case "":
		// upgradable: new installation
		// non-upgradable: error or non-supported version (v1alpha1 which cannot upgrade directly)
		upgradable, err := v1beta1.CanUpgrade(config, namespace)
		if err != nil {
			return err
		}

		if upgradable {
			crdAPIVersionSetting = &longhorn.Setting{
				ObjectMeta: metav1.ObjectMeta{
					Name: string(types.SettingNameCRDAPIVersion),
				},
				Value: types.CurrentCRDAPIVersion,
			}
			_, err = lhClient.LonghornV1beta2().Settings(namespace).Create(context.TODO(), crdAPIVersionSetting, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return errors.Wrap(err, "cannot create CRDAPIVersionSetting")
			}
			logrus.Infof("New %v installation", types.CurrentCRDAPIVersion)
		}
	case types.CRDAPIVersionV1beta1:
		logrus.Infof("Upgrading from %v to %v", types.CRDAPIVersionV1beta1, types.CurrentCRDAPIVersion)
		if err := v1beta1.UpgradeCRFromV1beta1ToV1beta2(config, namespace, lhClient); err != nil {
			return err
		}
		crdAPIVersionSetting.Value = types.CRDAPIVersionV1beta2
		if _, err := lhClient.LonghornV1beta2().Settings(namespace).Update(context.TODO(), crdAPIVersionSetting, metav1.UpdateOptions{}); err != nil {
			return errors.Wrapf(err, "cannot finish CRD API upgrade by setting the CRDAPIVersionSetting to %v", types.CurrentCRDAPIVersion)
		}
		logrus.Infof("CRD has been upgraded to %v", crdAPIVersionSetting.Value)
	default:
		return fmt.Errorf("don't support upgrade from %v to %v", crdAPIVersion, types.CurrentCRDAPIVersion)
	}

	return nil
}

func upgradeLocalNode() (err error) {
	defer func() {
		err = errors.Wrap(err, "upgrade local node failed")
	}()
	if err := v070to080.UpgradeLocalNode(); err != nil {
		return err
	}
	return nil
}

func doCRUpgrade(namespace string, lhClient *lhclientset.Clientset, kubeClient *clientset.Clientset) (err error) {
	defer func() {
		err = errors.Wrap(err, "upgrade CRD failed")
	}()
	if err := v070to080.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v100to101.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v102to110.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v110to111.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v110to120.UpgradeCRs(namespace, lhClient, kubeClient); err != nil {
		return err
	}
	if err := v111to120.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v120to121.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	if err := v122to123.UpgradeCRs(namespace, lhClient); err != nil {
		return err
	}
	return nil
}

func doPodsUpgrade(namespace string, lhClient *lhclientset.Clientset, kubeClient *clientset.Clientset) (err error) {
	defer func() {
		err = errors.Wrap(err, "upgrade Pods failed")
	}()
	if err = v100to101.UpgradeInstanceManagerPods(namespace, lhClient, kubeClient); err != nil {
		return err
	}
	if err = v102to110.UpgradePods(namespace, kubeClient); err != nil {
		return err
	}
	if err = v110to111.UpgradePods(namespace, lhClient, kubeClient); err != nil {
		return err
	}
	return nil
}

func doServicesUpgrade(namespace string, kubeClient *clientset.Clientset) (err error) {
	defer func() {
		err = errors.Wrap(err, "doServicesUpgrade failed")
	}()
	if err = v110to111.UpgradeServices(namespace, kubeClient); err != nil {
		return err
	}
	return nil
}

func doDeploymentAndDaemonSetUpgrade(namespace string, kubeClient *clientset.Clientset) (err error) {
	defer func() {
		err = errors.Wrap(err, "doDeploymentAndDaemonSetUpgrade failed")
	}()
	if err = v110to111.UpgradeDeploymentAndDaemonSet(namespace, kubeClient); err != nil {
		return err
	}
	return nil
}
