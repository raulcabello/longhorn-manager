package csi

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"
)

const (
	DefaultCSIAttacherImage            = "quay.io/k8scsi/csi-attacher:v2.0.0"
	DefaultCSIProvisionerImage         = "quay.io/k8scsi/csi-provisioner:v1.4.0"
	DefaultCSINodeDriverRegistrarImage = "quay.io/k8scsi/csi-node-driver-registrar:v1.2.0"

	DefaultCSIAttacherReplicaCount    = 3
	DefaultCSIProvisionerReplicaCount = 3

	DefaultInContainerKubeletRootDir      = "/var/lib/kubelet/"
	DefaultCSISocketFileName              = "csi.sock"
	DefaultOnHostCSIRegistrationDirSuffix = "/plugins_registry"
	DefaultInContainerCSISocketDir        = "/csi/"
	DefaultInContainerCSIRegistrationDir  = "/registration"
	DefaultCommonPluginsDirSuffix         = "/plugins/"

	AnnotationCSIVersion        = types.LonghornDriverName + "/version"
	AnnotationKubernetesVersion = types.LonghornDriverName + "/kubernetes-version"
)

var (
	HostPathDirectory             = v1.HostPathDirectory
	HostPathDirectoryOrCreate     = v1.HostPathDirectoryOrCreate
	MountPropagationBidirectional = v1.MountPropagationBidirectional
)

type AttacherDeployment struct {
	service    *v1.Service
	deployment *appsv1.Deployment
}

func NewAttacherDeployment(namespace, serviceAccount, attacherImage, rootDir string, replicaCount int, tolerations []v1.Toleration) *AttacherDeployment {
	service := getCommonService(types.CSIAttacherName, namespace)

	deployment := getCommonDeployment(
		types.CSIAttacherName,
		namespace,
		serviceAccount,
		attacherImage,
		rootDir,
		[]string{
			"--v=5",
			"--csi-address=$(ADDRESS)",
			"--leader-election",
			"--leader-election-namespace=$(POD_NAMESPACE)",
		},
		int32(replicaCount),
		tolerations,
	)

	return &AttacherDeployment{
		service:    service,
		deployment: deployment,
	}
}

func (a *AttacherDeployment) Deploy(kubeClient *clientset.Clientset) error {
	if err := deploy(kubeClient, a.service, "service",
		serviceCreateFunc, serviceDeleteFunc, serviceGetFunc); err != nil {
		return err
	}

	return deploy(kubeClient, a.deployment, "deployment",
		deploymentCreateFunc, deploymentDeleteFunc, deploymentGetFunc)
}

func (a *AttacherDeployment) Cleanup(kubeClient *clientset.Clientset) {
	var wg sync.WaitGroup
	defer wg.Wait()

	util.RunAsync(&wg, func() {
		if err := cleanup(kubeClient, a.service, "service",
			serviceDeleteFunc, serviceGetFunc); err != nil {
			logrus.Warnf("Failed to cleanup service in attacher deployment: %v", err)
		}
	})
	util.RunAsync(&wg, func() {
		if err := cleanup(kubeClient, a.deployment, "deployment",
			deploymentDeleteFunc, deploymentGetFunc); err != nil {
			logrus.Warnf("Failed to cleanup deployment in attacher deployment: %v", err)
		}
	})
}

type ProvisionerDeployment struct {
	service    *v1.Service
	deployment *appsv1.Deployment
}

func NewProvisionerDeployment(namespace, serviceAccount, provisionerImage, rootDir string, replicaCount int, tolerations []v1.Toleration) *ProvisionerDeployment {
	service := getCommonService(types.CSIProvisionerName, namespace)

	deployment := getCommonDeployment(
		types.CSIProvisionerName,
		namespace,
		serviceAccount,
		provisionerImage,
		rootDir,
		[]string{
			"--v=5",
			"--csi-address=$(ADDRESS)",
			"--enable-leader-election",
			"--leader-election-type=leases",
			"--leader-election-namespace=$(POD_NAMESPACE)",
		},
		int32(replicaCount),
		tolerations,
	)

	return &ProvisionerDeployment{
		service:    service,
		deployment: deployment,
	}
}

func (p *ProvisionerDeployment) Deploy(kubeClient *clientset.Clientset) error {
	if err := deploy(kubeClient, p.service, "service",
		serviceCreateFunc, serviceDeleteFunc, serviceGetFunc); err != nil {
		return err
	}

	return deploy(kubeClient, p.deployment, "deployment",
		deploymentCreateFunc, deploymentDeleteFunc, deploymentGetFunc)
}

func (p *ProvisionerDeployment) Cleanup(kubeClient *clientset.Clientset) {
	var wg sync.WaitGroup
	defer wg.Wait()

	util.RunAsync(&wg, func() {
		if err := cleanup(kubeClient, p.service, "service",
			serviceDeleteFunc, serviceGetFunc); err != nil {
			logrus.Warnf("Failed to cleanup service in provisioner deployment: %v", err)
		}
	})
	util.RunAsync(&wg, func() {
		if err := cleanup(kubeClient, p.deployment, "deployment",
			deploymentDeleteFunc, deploymentGetFunc); err != nil {
			logrus.Warnf("Failed to cleanup deployment in provisioner deployment: %v", err)
		}
	})
}

type PluginDeployment struct {
	daemonSet *appsv1.DaemonSet
}

func NewPluginDeployment(namespace, serviceAccount, nodeDriverRegistrarImage, managerImage, managerURL, rootDir string, tolerations []v1.Toleration) *PluginDeployment {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      types.CSIPluginName,
			Namespace: namespace,
		},

		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": types.CSIPluginName,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": types.CSIPluginName,
					},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: serviceAccount,
					Tolerations:        tolerations,
					Containers: []v1.Container{
						{
							Name:  "node-driver-registrar",
							Image: nodeDriverRegistrarImage,
							Lifecycle: &v1.Lifecycle{
								PreStop: &v1.Handler{
									Exec: &v1.ExecAction{
										Command: []string{
											"/bin/sh", "-c",
											fmt.Sprintf("rm -rf %s/%s %s/%s-reg.sock %s/*", GetInContainerCSIRegistrationDir(), types.LonghornDriverName, GetInContainerCSIRegistrationDir(), types.LonghornDriverName, GetInContainerCSISocketDir()),
										},
									},
								},
							},
							SecurityContext: &v1.SecurityContext{
								Privileged: pointer.BoolPtr(true),
							},
							Args: []string{
								"--v=5",
								"--csi-address=$(ADDRESS)",
								"--kubelet-registration-path=" + GetOnHostCSISocketFilePath(rootDir),
							},
							Env: []v1.EnvVar{
								{
									Name:  "ADDRESS",
									Value: GetInContainerCSISocketFilePath(),
								},
							},
							//ImagePullPolicy: v1.PullAlways,
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "socket-dir",
									MountPath: GetInContainerCSISocketDir(),
								},
								{
									Name:      "registration-dir",
									MountPath: GetInContainerCSIRegistrationDir(),
								},
							},
						},
						{
							Name: "longhorn-csi-plugin",
							SecurityContext: &v1.SecurityContext{
								Privileged: pointer.BoolPtr(true),
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{
										"SYS_ADMIN",
									},
								},
								AllowPrivilegeEscalation: pointer.BoolPtr(true),
							},
							Image: managerImage,
							Lifecycle: &v1.Lifecycle{
								PreStop: &v1.Handler{
									Exec: &v1.ExecAction{
										Command: []string{
											"/bin/sh", "-c",
											fmt.Sprintf("rm -f %s/*", GetInContainerCSISocketDir()),
										},
									},
								},
							},
							Args: []string{
								"longhorn-manager",
								"-d",
								"csi",
								"--nodeid=$(NODE_ID)",
								"--endpoint=$(CSI_ENDPOINT)",
								fmt.Sprintf("--drivername=%s", types.LonghornDriverName),
								"--manager-url=" + managerURL,
							},
							Env: []v1.EnvVar{
								{
									Name: "NODE_ID",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name:  "CSI_ENDPOINT",
									Value: GetCSIEndpoint(),
								},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:             "plugin-dir",
									MountPath:        GetInContainerPluginsDir(),
									MountPropagation: &MountPropagationBidirectional,
								},
								{
									Name:      "socket-dir",
									MountPath: GetInContainerCSISocketDir(),
								},
								{
									Name:             "pods-mount-dir",
									MountPath:        filepath.Join(rootDir, "/pods"),
									MountPropagation: &MountPropagationBidirectional,
								},
								{
									Name:      "host-dev",
									MountPath: "/dev",
								},
								{
									Name:      "host-sys",
									MountPath: "/sys",
								},
								{
									Name:      "lib-modules",
									MountPath: "/lib/modules",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "registration-dir",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: GetOnHostCSIRegistrationDir(rootDir),
									Type: &HostPathDirectory,
								},
							},
						},
						{
							Name: "socket-dir",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: GetOnHostCSISocketDir(rootDir),
									Type: &HostPathDirectoryOrCreate,
								},
							},
						},
						{
							Name: "plugin-dir",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: GetOnHostPluginsDir(rootDir),
									Type: &HostPathDirectory,
								},
							},
						},
						{
							Name: "pods-mount-dir",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: filepath.Join(rootDir, "/pods"),
									Type: &HostPathDirectoryOrCreate,
								},
							},
						},
						{
							Name: "host-dev",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/dev",
								},
							},
						},
						{
							Name: "host-sys",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/sys",
								},
							},
						},
						{
							Name: "lib-modules",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/lib/modules",
								},
							},
						},
					},
				},
			},
		},
	}

	return &PluginDeployment{
		daemonSet: daemonSet,
	}
}

func (p *PluginDeployment) Deploy(kubeClient *clientset.Clientset) error {
	return deploy(kubeClient, p.daemonSet, "daemon set",
		daemonSetCreateFunc, daemonSetDeleteFunc, daemonSetGetFunc)
}

func (p *PluginDeployment) Cleanup(kubeClient *clientset.Clientset) {
	if err := cleanup(kubeClient, p.daemonSet, "daemon set",
		daemonSetDeleteFunc, daemonSetGetFunc); err != nil {
		logrus.Warnf("Failed to cleanup DaemonSet in plugin deployment: %v", err)
	}
}
