package deploypipeline

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	dns "google.golang.org/api/dns/v2beta1"
	"istio.io/api/networking/v1alpha3"
	networkingv1alpha3 "istio.io/client-go/pkg/apis/networking/v1alpha3"
	versionedclient "istio.io/client-go/pkg/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

type Status struct {
	Message string
}

type Networking struct {
	Port        uint   `json:"port"`
	ExposedPort uint   `json:"exposed_port"`
	Protocol    string `json:"protocol"`
	Exposed     bool   `json:"exposed"`
}

// Volume Size units are M (megabytes)
type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	Size      uint   `json:"size"`
}

// MaxCPU units are millicpu 1/1000 of CPU unit
// MaxRAM units are Mi (mebibyte)
// MaxHDD units are in Mi (mebibyte)
// See: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/
// This is Basic Autoscale on the front end
type VerticalPodScaleLimit struct {
	AllocatedCPU uint `json:"allocatedCPU"`
	ScaledCPU    uint `json:"scaledCPU"`
	AllocatedRAM uint `json:"allocatedRAM"`
	ScaledRAM    uint `json:"scaledRAM"`
	AllocatedHDD uint `json:"allocatedHDD"`
}

// TargetCPU units are millicpu 1/1000 of CPU unit
// TargetRAM units are Mi (mebibyte)
// TargetHDD units are in Mi (mebibyte)
// This is Advanced Autoscale on the front end
type HorizontalPodScaleLimit struct {
	Instances uint `json:"instances"`
	TargetCPU uint `json:"targetCPU"`
	TargetRAM uint `json:"targetRAM"`
	TargetHDD uint `json:"targetHDD"`
}

type CustomerData struct {
	AppName                  string
	CustomerName             string
	CustomerProjectName      string
	CustomerServiceName      string
	CustomerOrganizationName string
	RegistryUrl              string
	DockerImageName          string
	DockerRegistrySecretPath string
	ServiceAccountPath       string
	Namespace                string
	Networking               []Networking
	Volumes                  []Volume
	VerticalPodScaleLimit    VerticalPodScaleLimit
	HorizontalPodScaleLimit  HorizontalPodScaleLimit
	EnvironmentVariables     map[string]string
	DeployId                 string
	IstioGatewayIP           string
	Replicas                 int32
	PublicAccessible         bool
}

func int32Ptr(i int32) *int32 { return &i }

func GetNamespace(namespace string, clientset *kubernetes.Clientset) (*corev1.Namespace, error) {
	return clientset.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})
}

func CreateNamespace(namespace string, clientset *kubernetes.Clientset) (*corev1.Namespace, Status, error) {
	status := Status{Message: ""}

	nsName := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}

	namespaceRef, err := clientset.CoreV1().Namespaces().Create(context.Background(), nsName, metav1.CreateOptions{})
	if err != nil {
		status.Message = "Namespace creation error"
		return nil, status, errCreateNamespace()
	}

	status.Message = "Namespace creation succeeded"

	return namespaceRef, status, nil
}

func DeleteNamespace(namespace string, clientset *kubernetes.Clientset) (Status, error) {
	status := Status{Message: ""}

	namespaceDeleteErr := clientset.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
	if namespaceDeleteErr != nil {
		status.Message = "Namespace deletion error"
		return status, namespaceDeleteErr
	}

	status.Message = "Namespace successfully deleted"
	return status, nil
}

func InitializeSecrets(clientset *kubernetes.Clientset, customer CustomerData) (Status, error) {
	status := Status{Message: ""}

	nameBaseLowercase := strings.ToLower(customer.CustomerName + "-" + customer.CustomerProjectName + "-" + customer.AppName)
	nameBase := strings.Replace(nameBaseLowercase, " ", "-", -1)

	// create secret for Docker registry auth
	createDockerSecretStatus, createDockerSecretErr := CreateDockerRegistrySecret(clientset, customer.Namespace, customer.DockerRegistrySecretPath, customer, nameBase)

	if createDockerSecretErr != nil {
		return createDockerSecretStatus, createDockerSecretErr
	}

	// create 'build-packs-service' service account
	createServiceAccountStatus, createServiceAccountErr := CreateServiceAccount(clientset, customer.Namespace, customer.ServiceAccountPath, customer, nameBase)
	if createServiceAccountErr != nil {
		return createServiceAccountStatus, createServiceAccountErr
	}

	status.Message = "Secrets Initialized Successfully"

	return status, nil
}

func CreateDeployment(customer CustomerData, clientset *kubernetes.Clientset, istioclientset *versionedclient.Clientset) (Status, <-chan watch.Event, <-chan watch.Event, []string, error) {
	status := Status{Message: ""}
	namespaceLowercase := strings.ToLower(customer.Namespace)
	namespace := strings.Replace(namespaceLowercase, " ", "-", -1)
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	customerServiceName := customer.CustomerServiceName
	customerOrgName := customer.CustomerOrganizationName
	deployId := customer.DeployId
	appName := customer.AppName
	registryUrl := customer.RegistryUrl
	imageName := customer.DockerImageName
	fullImageName := registryUrl + "/" + imageName
	nameBaseLowercase := strings.ToLower(customerName + "-" + customerProjectName + "-" + appName)
	nameBase := strings.Replace(nameBaseLowercase, " ", "-", -1)
	istioGatewayIP := customer.IstioGatewayIP
	replicas := customer.Replicas
	publicAccessible := customer.PublicAccessible

	reqCPU, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.AllocatedCPU), 10) + "m")
	reqRAM, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.AllocatedRAM), 10) + "Mi")
	maxCPU, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.ScaledCPU), 10) + "m")
	maxRAM, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.ScaledRAM), 10) + "Mi")
	maxHDD, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.AllocatedHDD), 10) + "Mi")

	if parseErr != nil {
		status = Status{Message: "error parsing units"}
		return status, nil, nil, []string{}, parseErr
	}

	var containerPorts []corev1.ContainerPort
	var servicePorts []corev1.ServicePort

	addedPorts := make(map[int]int)

	for i, n := range customer.Networking {
		validProtocol(n.Protocol)
		portPrefix := "tcp-port-"
		containerProtocol := "TCP"
		if strings.ToLower(n.Protocol) == "http" {
			portPrefix = "http-port-"
		}
		if _, exists := addedPorts[int(n.Port)]; !exists {
			servicePorts = append(servicePorts, corev1.ServicePort{
				Name: portPrefix + "service-" + strconv.Itoa(i),
				Port: int32(n.Port),
			})
			containerPorts = append(containerPorts, corev1.ContainerPort{
				Name:          portPrefix + strconv.Itoa(i),
				ContainerPort: int32(n.Port),
				Protocol:      corev1.Protocol(containerProtocol),
			})
			addedPorts[int(n.Port)] = 1
		}

		// we can add this to ensure there will be url creation/exposed if at least one port is marked as exposed/external
		if n.Exposed {
			publicAccessible = true
		}
	}

	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	for _, cv := range customer.Volumes {
		volSize, parseErr := resource.ParseQuantity(strconv.FormatUint(uint64(cv.Size), 10) + "Mi")

		if parseErr != nil {
			status = Status{Message: "error parsing units"}
			return status, nil, nil, []string{}, parseErr
		}

		claimName := "pvc-" + strings.ToLower(strings.Replace(cv.Name, " ", "-", -1))
		pvc := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claimName,
				Namespace: namespace,
				Labels: map[string]string{
					"domenetwork.io/appName":      appName,
					"domenetwork.io/customer":     customerName,
					"domenetwork.io/organization": customerOrgName,
					"domenetwork.io/project":      customerProjectName,
					"domenetwork.io/service":      customerServiceName,
				},
			},

			Spec: corev1.PersistentVolumeClaimSpec{
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceStorage: maxHDD,
					},

					Requests: corev1.ResourceList{
						corev1.ResourceStorage: volSize,
					},
				},

				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
			},
		}

		_, pvcErr := clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), &pvc, metav1.CreateOptions{})

		if pvcErr != nil {
			status = Status{Message: "error creating persistent volume claim"}
			return status, nil, nil, []string{}, pvcErr
		}

		volumes = append(volumes, corev1.Volume{
			Name: strings.ToLower(cv.Name),
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				},
			},
		})

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      strings.ToLower(cv.Name),
			MountPath: cv.MountPath,
		})
	}

	var envVars []corev1.EnvVar

	for k, v := range customer.EnvironmentVariables {
		envVars = append(envVars, corev1.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	deploymentsClient := clientset.AppsV1().Deployments(namespace)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: customer.Namespace,
			Labels: map[string]string{
				"domenetwork.io/appName":      appName,
				"domenetwork.io/customer":     customerName,
				"domenetwork.io/organization": customerOrgName,
				"domenetwork.io/project":      customerProjectName,
				"domenetwork.io/service":      customerServiceName,
				"domenetwork.io/deployId":     deployId,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"domenetwork.io/appName":      appName,
					"domenetwork.io/customer":     customerName,
					"domenetwork.io/organization": customerOrgName,
					"domenetwork.io/project":      customerProjectName,
					"domenetwork.io/service":      customerServiceName,
					"domenetwork.io/deployId":     deployId,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"domenetwork.io/appName":      appName,
						"domenetwork.io/customer":     customerName,
						"domenetwork.io/organization": customerOrgName,
						"domenetwork.io/project":      customerProjectName,
						"domenetwork.io/service":      customerServiceName,
						"domenetwork.io/deployId":     deployId,
					},
					Annotations: map[string]string{
						"deployTime": time.Now().String(),
					},
				},
				Spec: corev1.PodSpec{
					Volumes:            volumes,
					ImagePullSecrets:   []corev1.LocalObjectReference{},
					ServiceAccountName: "deploy-service-account-" + nameBase,
					Containers: []corev1.Container{
						{
							Name:            appName,
							Image:           fullImageName,
							ImagePullPolicy: corev1.PullAlways,
							Env:             envVars,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    maxCPU,
									corev1.ResourceMemory: maxRAM,
								},

								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    reqCPU,
									corev1.ResourceMemory: reqRAM,
								},
							},
							VolumeMounts: volumeMounts,
							Ports:        containerPorts,
						},
					},
				},
			},
		},
	}

	serviceClient := clientset.CoreV1().Services(customer.Namespace)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: customer.Namespace,
			Labels: map[string]string{
				"domenetwork.io/appName":      appName,
				"domenetwork.io/customer":     customerName,
				"domenetwork.io/organization": customerOrgName,
				"domenetwork.io/project":      customerProjectName,
				"domenetwork.io/service":      customerServiceName,
			},
			Annotations: map[string]string{
				"deployTime": time.Now().String(),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: servicePorts,
			Selector: map[string]string{
				"domenetwork.io/appName": appName,
			},
			Type:            corev1.ServiceTypeClusterIP,
			SessionAffinity: corev1.ServiceAffinityNone,
		},
	}

	// Create Deployment
	fmt.Println("Creating deployment...")
	result, deploymentErr := deploymentsClient.Create(context.TODO(), deployment, metav1.CreateOptions{})
	if deploymentErr != nil {
		return status, nil, nil, []string{}, deploymentErr
	}

	fmt.Printf("Created deployment %q.\n", result.GetObjectMeta().GetName())

	// Create Service
	fmt.Println("Creating service...")
	_, serviceErr := serviceClient.Create(context.TODO(), service, metav1.CreateOptions{})
	if serviceErr != nil {
		return status, nil, nil, []string{}, serviceErr
	}

	// Expose it
	var mainDeployUrl string
	if publicAccessible {
		// deployment needs to be exposed
		createStatus, serviceUrl, err := CreateDeployURL(namespace, customerOrgName, customerProjectName, customerServiceName, istioGatewayIP)
		if err != nil {
			return createStatus, nil, nil, []string{}, err
		}
		mainDeployUrl = serviceUrl

		exposeStatus, _, err := ExposeDeployment(namespace, appName, "", customerName, customerOrgName, customerProjectName, customerServiceName, serviceUrl, istioGatewayIP, customer.Networking, istioclientset)

		if err != nil {
			return exposeStatus, nil, nil, []string{}, err
		}

	}
	var deployUrls []string

	for _, n := range customer.Networking {
		if n.Exposed {
			if n.Protocol == "http" {
				deployUrls = append(deployUrls, "https://"+mainDeployUrl+":"+strconv.Itoa(int(n.ExposedPort)))
			} else {
				deployUrls = append(deployUrls, mainDeployUrl+":"+strconv.Itoa(int(n.ExposedPort)))
			}

		}
	}

	watchTimeout := int64(300)
	watchLabel := map[string]string{
		"domenetwork.io/appName": appName,
	}

	deploymentWatchInterface, deploymentWatchChannelErr := deploymentsClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	serviceWatchInterface, serviceWatchChannelErr := serviceClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	if deploymentWatchChannelErr != nil {
		return status, nil, nil, []string{}, deploymentWatchChannelErr
	}

	if serviceWatchChannelErr != nil {
		return status, nil, nil, []string{}, serviceWatchChannelErr
	}

	deploymentWatchChannel := deploymentWatchInterface.ResultChan()
	serviceWatchChannel := serviceWatchInterface.ResultChan()

	status.Message = "Deployment and virtual service creation succeeded."
	return status, deploymentWatchChannel, serviceWatchChannel, deployUrls, nil
}

func DeleteDeployment(customer CustomerData, clientset *kubernetes.Clientset, istioclientset *versionedclient.Clientset) (Status, <-chan watch.Event, <-chan watch.Event, error) {
	status := Status{Message: ""}

	deploymentsClient := clientset.AppsV1().Deployments(customer.Namespace)
	serviceClient := clientset.CoreV1().Services(customer.Namespace)
	virtualServiceClient := istioclientset.NetworkingV1alpha3().VirtualServices(customer.Namespace)
	deletePolicy := metav1.DeletePropagationForeground

	fmt.Println("Deleting deployment...")
	if deleteDeploymentErr := deploymentsClient.Delete(context.TODO(), customer.AppName, metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); deleteDeploymentErr != nil {
		return status, nil, nil, deleteDeploymentErr
	}

	fmt.Println("Deleting internal service...")
	if deleteServiceErr := serviceClient.Delete(context.TODO(), customer.AppName, metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); deleteServiceErr != nil {
		return status, nil, nil, deleteServiceErr
	}

	fmt.Println("Deleting istio virtual service...")
	if deleteVirtualServiceErr := virtualServiceClient.Delete(context.TODO(), customer.AppName+"-virtualservice", metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); deleteVirtualServiceErr != nil {
		return status, nil, nil, deleteVirtualServiceErr
	}

	watchTimeout := int64(300)
	watchLabel := map[string]string{
		"domenetwork.io/appName": customer.AppName,
	}

	deploymentWatchInterface, deploymentWatchChannelErr := deploymentsClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	serviceWatchInterface, serviceWatchChannelErr := serviceClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	if deploymentWatchChannelErr != nil {
		return status, nil, nil, deploymentWatchChannelErr
	}

	if serviceWatchChannelErr != nil {
		return status, nil, nil, serviceWatchChannelErr
	}

	teardownDeploymentWatchChannel := deploymentWatchInterface.ResultChan()
	teardownServiceWatchChannel := serviceWatchInterface.ResultChan()

	status.Message = "Deployment and external service deletion succeeded."
	return status, teardownDeploymentWatchChannel, teardownServiceWatchChannel, nil
}

func RedeployDeployment(customer CustomerData, clientset *kubernetes.Clientset) (Status, <-chan watch.Event, error) {
	status := Status{Message: ""}

	deploymentsClient := clientset.AppsV1().Deployments(customer.Namespace)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := deploymentsClient.Get(context.TODO(), customer.AppName, metav1.GetOptions{})
		if getErr != nil {
			panic(fmt.Errorf("Failed to get latest version of Deployment: %v", getErr))
		}

		result.Spec.Template.ObjectMeta.SetAnnotations(map[string]string{
			"deployTime": time.Now().String(),
		})

		_, updateErr := deploymentsClient.Update(context.TODO(), result, metav1.UpdateOptions{})
		return updateErr
	})

	if retryErr != nil {
		status.Message = "Redeploy failed"
		return status, nil, retryErr
	}

	watchTimeout := int64(300)
	watchLabel := map[string]string{
		"domenetwork.io/appName": customer.AppName,
	}

	deploymentWatchInterface, deploymentWatchChannelErr := deploymentsClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	if deploymentWatchChannelErr != nil {
		return status, nil, deploymentWatchChannelErr
	}

	deploymentWatchChannel := deploymentWatchInterface.ResultChan()

	status.Message = "Successfully redeployed deployment"
	return status, deploymentWatchChannel, nil
}

func UpdateDeployment(customer CustomerData, clientset *kubernetes.Clientset, istioclientset *versionedclient.Clientset) (Status, <-chan watch.Event, <-chan watch.Event, error) {
	status := Status{Message: ""}

	deploymentsClient := clientset.AppsV1().Deployments(customer.Namespace)
	serviceClient := clientset.CoreV1().Services(customer.Namespace)
	virtualServiceClient := istioclientset.NetworkingV1alpha3().VirtualServices(customer.Namespace)

	retryDeploymentErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentDeployment, getDeploymentErr := deploymentsClient.Get(context.TODO(), customer.AppName, metav1.GetOptions{})

		if getDeploymentErr != nil {
			status = Status{Message: "failed to get latest version of Deployment - update deployment"}
			return getDeploymentErr
		}

		var envVars []corev1.EnvVar
		for k, v := range customer.EnvironmentVariables {
			envVars = append(envVars, corev1.EnvVar{
				Name:  k,
				Value: v,
			})
		}

		var containerPorts []corev1.ContainerPort
		addedPorts := make(map[int]int)

		for i, n := range customer.Networking {
			validProtocol(n.Protocol)
			containerProtocol := "TCP"
			if _, exists := addedPorts[int(n.Port)]; !exists {
				containerPorts = append(containerPorts, corev1.ContainerPort{
					Name:          "tcp-port-" + strconv.Itoa(i),
					ContainerPort: int32(n.Port),
					Protocol:      corev1.Protocol(containerProtocol),
				})
				addedPorts[int(n.Port)] = 1
			}
		}

		var volumes []corev1.Volume
		var volumeMounts []corev1.VolumeMount

		currentPVCs, err := clientset.CoreV1().PersistentVolumeClaims(customer.Namespace).List(context.TODO(), metav1.ListOptions{})

		if err != nil {
			status = Status{Message: "error fetching volumes"}
			return err
		}

		for index, cv := range customer.Volumes {
			claimName := "pvc-" + strings.ToLower(strings.Replace(cv.Name, " ", "-", -1))
			volSize, _ := resource.ParseQuantity(strconv.FormatUint(uint64(cv.Size), 10) + "Mi")
			maxHDD, _ := resource.ParseQuantity(strconv.FormatUint(uint64(customer.VerticalPodScaleLimit.AllocatedHDD), 10) + "Mi")
			storageSize := resource.NewQuantity(int64(cv.Size*1024*1024), resource.BinarySI)
			currentPVCSize := len(currentPVCs.Items)

			// if the volume already exist then compare their current sizes to make sure it has not been updated ?
			if currentPVCSize > 0 && index < currentPVCSize {
				val := currentPVCs.Items[index].Spec.Resources.Requests.Storage().ToDec()
				// If the size has been upgraded, update the claim and append to the list of volumes
				if claimName == currentPVCs.Items[index].Name && storageSize.Cmp(resource.MustParse(val.String())) == 1 {
					pvc, err := clientset.CoreV1().PersistentVolumeClaims(customer.Namespace).Get(context.TODO(), claimName, metav1.GetOptions{})

					if err != nil {
						status = Status{Message: "failed to fetch the existing persistent volume claim"}
						return err
					}

					pvc.Spec.Resources.Requests = corev1.ResourceList{
						corev1.ResourceStorage: volSize,
					}

					_, pvcErr := clientset.CoreV1().PersistentVolumeClaims(customer.Namespace).Update(context.TODO(), pvc, metav1.UpdateOptions{})

					if pvcErr != nil {
						status = Status{Message: "error updating persistent volume claim"}
						return pvcErr
					}

					volumes = append(volumes, corev1.Volume{
						Name: strings.ToLower(cv.Name),
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: claimName,
							},
						},
					})

					volumeMounts = append(volumeMounts, corev1.VolumeMount{
						Name:      strings.ToLower(cv.Name),
						MountPath: cv.MountPath,
					})
				} else {
					// Just append to the volume without updating
					volumes = append(volumes, corev1.Volume{
						Name: strings.ToLower(cv.Name),
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: claimName,
							},
						},
					})

					volumeMounts = append(volumeMounts, corev1.VolumeMount{
						Name:      strings.ToLower(cv.Name),
						MountPath: cv.MountPath,
					})
				}
			} else {
				// if its a new claim then create it
				pvc := corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      claimName,
						Namespace: customer.Namespace,
						Labels: map[string]string{
							"domenetwork.io/appName":      customer.AppName,
							"domenetwork.io/customer":     customer.CustomerName,
							"domenetwork.io/organization": customer.CustomerOrganizationName,
							"domenetwork.io/project":      customer.CustomerProjectName,
							"domenetwork.io/service":      customer.CustomerServiceName,
						},
					},

					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceStorage: maxHDD,
							},

							Requests: corev1.ResourceList{
								corev1.ResourceStorage: volSize,
							},
						},

						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
					},
				}

				_, pvcErr := clientset.CoreV1().PersistentVolumeClaims(customer.Namespace).Create(context.TODO(), &pvc, metav1.CreateOptions{})

				if pvcErr != nil {
					status = Status{Message: "error creating persistent volume claim"}
					return pvcErr
				}

				volumes = append(volumes, corev1.Volume{
					Name: strings.ToLower(cv.Name),
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
						},
					},
				})

				volumeMounts = append(volumeMounts, corev1.VolumeMount{
					Name:      strings.ToLower(cv.Name),
					MountPath: cv.MountPath,
				})
			}
		}

		podSpec := currentDeployment.Spec.Template.Spec
		podSpec.Volumes = volumes

		// set the updated podSpec back on the current deployment
		currentDeployment.Spec.Template.Spec = podSpec

		containers := currentDeployment.Spec.Template.Spec.Containers
		for i := range containers {
			currentDeployment.Spec.Template.Spec.Containers[i].Env = envVars
			currentDeployment.Spec.Template.Spec.Containers[i].Ports = containerPorts
			currentDeployment.Spec.Template.Spec.Containers[i].VolumeMounts = volumeMounts
		}

		currentDeployment.Spec.Template.ObjectMeta.SetAnnotations(map[string]string{
			"deployTime": time.Now().String(),
		})

		_, deploymentUpdateErr := deploymentsClient.Update(context.TODO(), currentDeployment, metav1.UpdateOptions{})

		return deploymentUpdateErr
	})

	if retryDeploymentErr != nil {
		status.Message = "Update Deployment failed"
		return status, nil, nil, retryDeploymentErr
	}

	retryServiceErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Service before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		currentService, getServiceErr := serviceClient.Get(context.TODO(), customer.AppName, metav1.GetOptions{})

		if getServiceErr != nil {
			status = Status{Message: "failed to get the latest version of the service - update deployment"}
			return getServiceErr
		}

		var servicePorts []corev1.ServicePort
		addedPorts := make(map[int]int)

		for i, n := range customer.Networking {
			if _, exists := addedPorts[int(n.Port)]; !exists {
				portPrefix := "tcp-port-"
				if strings.ToLower(n.Protocol) == "http" {
					portPrefix = "http-port-"
				}
				servicePorts = append(servicePorts, corev1.ServicePort{
					Name: portPrefix + "service-" + strconv.Itoa(i),
					Port: int32(n.Port),
				})
				addedPorts[int(n.Port)] = 1
			}
		}

		currentService.Spec.Ports = servicePorts

		currentService.ObjectMeta.SetAnnotations(map[string]string{
			"deployTime": time.Now().String(),
		})

		_, serviceUpdateErr := serviceClient.Update(context.TODO(), currentService, metav1.UpdateOptions{})
		return serviceUpdateErr
	})

	if retryServiceErr != nil {
		status.Message = "Update Service failed"
		return status, nil, nil, retryServiceErr
	}

	currentVirtualService, getvirtualServiceErr := virtualServiceClient.Get(context.TODO(), customer.AppName+"-virtualservice", metav1.GetOptions{})

	if getvirtualServiceErr != nil {
		status = Status{Message: "failed to get the latest virtual version of the service - update deployment"}
		return status, nil, nil, getvirtualServiceErr
	}

	for _, n := range customer.Networking {
		if n.Exposed {
			if strings.ToLower(n.Protocol) == "tcp" {
				/* if internalHostName != "" {
					destinationHost = internalHostName
				} */
				currentVirtualService.Spec.Tcp = append(currentVirtualService.Spec.Tcp, &v1alpha3.TCPRoute{
					Match: []*v1alpha3.L4MatchAttributes{
						{
							Port: uint32(n.ExposedPort),
						},
					},
					Route: []*v1alpha3.RouteDestination{
						{
							Destination: &v1alpha3.Destination{
								Host: customer.AppName,
								Port: &v1alpha3.PortSelector{
									Number: uint32(n.Port),
								},
							},
						},
					},
				})
			} else {
				currentVirtualService.Spec.Http = append(currentVirtualService.Spec.Http, &v1alpha3.HTTPRoute{
					Match: []*v1alpha3.HTTPMatchRequest{
						{
							Port: uint32(n.ExposedPort),
							Uri: &v1alpha3.StringMatch{
								MatchType: &v1alpha3.StringMatch_Prefix{
									Prefix: "/",
								},
							},
						},
					},
					Rewrite: &v1alpha3.HTTPRewrite{
						Uri: "/",
					},
					Route: []*v1alpha3.HTTPRouteDestination{
						{
							Destination: &v1alpha3.Destination{
								Host: customer.AppName,
								Port: &v1alpha3.PortSelector{
									Number: uint32(n.Port),
								},
							},
						},
					},
					Headers: &v1alpha3.Headers{
						Response: &v1alpha3.Headers_HeaderOperations{
							Set: map[string]string{"Strict-Transport-Security": "max-age=31536000"},
							Remove: []string{
								"x-envoy-upstream-healthchecked-cluster",
								"x-envoy-upstream-service-time",
								"Server",
								"Server",
							},
						},
					},
					CorsPolicy: &v1alpha3.CorsPolicy{
						AllowOrigins: []*v1alpha3.StringMatch{
							{
								MatchType: &v1alpha3.StringMatch_Regex{
									Regex: ".*",
								},
							},
						},
						AllowMethods: []string{
							"GET",
							"POST",
							"PUT",
							"DELETE",
							"PATCH",
							"HEAD",
							"OPTIONS",
						},
						AllowHeaders: []string{
							"*",
						},
					},
				})
			}
		}
	}

	_, vsErr := virtualServiceClient.Update(context.TODO(), currentVirtualService, metav1.UpdateOptions{})
	if vsErr != nil {
		status = Status{Message: "error creating an istio virtual service client"}
		return status, nil, nil, vsErr
	}

	watchTimeout := int64(300)
	watchLabel := map[string]string{
		"domenetwork.io/appName": customer.AppName,
	}

	deploymentWatchInterface, deploymentWatchChannelErr := deploymentsClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	serviceWatchInterface, serviceWatchChannelErr := serviceClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	if deploymentWatchChannelErr != nil {
		return status, nil, nil, deploymentWatchChannelErr
	}

	if serviceWatchChannelErr != nil {
		return status, nil, nil, serviceWatchChannelErr
	}

	deploymentWatchChannel := deploymentWatchInterface.ResultChan()
	serviceWatchChannel := serviceWatchInterface.ResultChan()

	status.Message = "Successfully redeployed deployment"
	return status, deploymentWatchChannel, serviceWatchChannel, nil
}

func GetDeployment(namespace string, appName string, clientset *kubernetes.Clientset) (Status, *appsv1.Deployment, error) {
	status := Status{Message: ""}

	deploymentsClient := clientset.AppsV1().Deployments(namespace)
	result, getErr := deploymentsClient.Get(context.TODO(), appName, metav1.GetOptions{})

	if getErr != nil {
		status.Message = "Get Deployment failed"
		return status, nil, getErr
	}

	status.Message = "Get Deployment successful"
	return status, result, getErr
}

func ScaleDeployment(customer CustomerData, clientset *kubernetes.Clientset, replicaSets int32) (Status, error) {
	status := Status{Message: ""}

	deploymentsClient := clientset.AppsV1().Deployments(customer.Namespace)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := deploymentsClient.Get(context.TODO(), customer.AppName, metav1.GetOptions{})
		if getErr != nil {
			panic(fmt.Errorf("Failed to get latest version of Deployment: %v", getErr))
		}

		result.Spec.Replicas = int32Ptr(replicaSets) // modify replica count
		_, updateErr := deploymentsClient.Update(context.TODO(), result, metav1.UpdateOptions{})
		return updateErr
	})

	if retryErr != nil {
		status.Message = "Update failed"
		return status, retryErr
	}

	status.Message = "Successfully scaled deployment"
	return status, nil
}

func CreateDockerRegistrySecret(clientset *kubernetes.Clientset, namespace string, dockerRegistrySecretPath string, customer CustomerData, nameBase string) (Status, error) {
	status := Status{Message: ""}

	// load the file into a buffer
	secretData, err := ioutil.ReadFile(dockerRegistrySecretPath)
	if err != nil {
		status.Message = "Create Docker registry secret error: read file"
		return status, err
	}

	// convert yaml to JSON
	secretJSON, yamlToJSONErr := yaml.ToJSON(secretData)
	if yamlToJSONErr != nil {
		status.Message = "Create Docker registry secret error: convert yaml to JSON"
		return status, yamlToJSONErr
	}

	var secret *corev1.Secret

	// convert JSON to Secret type
	jsonUnmarshalErr := json.Unmarshal(secretJSON, &secret)
	if jsonUnmarshalErr != nil {
		status.Message = "Create Docker registry secret error: unmarshal"
		return status, jsonUnmarshalErr
	}

	secret.Namespace = namespace
	secret.Name = "docker-user-pass-" + nameBase

	// set labels
	secret.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	secret.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	secret.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	secret.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)

	// create secret on cluster
	_, createDockerSecret := clientset.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if createDockerSecret != nil {
		status.Message = "Create Docker registry secret error"
		return status, createDockerSecret
	}

	status.Message = "Create Docker registry secret succeeded"

	return status, nil
}

func DeleteDockerRegistrySecret(clientset *kubernetes.Clientset, namespace string, nameBase string) (Status, error) {
	status := Status{Message: ""}

	secretName := "docker-user-pass-" + nameBase

	// delete secret on cluster
	deleteDockerSecret := clientset.CoreV1().Secrets(namespace).Delete(context.TODO(), secretName, metav1.DeleteOptions{})
	if deleteDockerSecret != nil {
		status.Message = "Docker registry secret deletion error"
		return status, deleteDockerSecret
	}

	status.Message = "Docker registry secret succeesfully deleted"

	return status, nil
}

func CreateServiceAccount(clientset *kubernetes.Clientset, namespace string, serviceAccountPath string, customer CustomerData, nameBase string) (Status, error) {
	serviceAccountData, readYamlError := readYamlFile(serviceAccountPath)
	status := Status{Message: ""}

	if readYamlError != nil {
		status.Message = "Create service account error: read yaml error"
		return status, readYamlError
	}

	// convert yaml to JSON
	serviceAccountJSON, yamlToJSONErr := yaml.ToJSON(serviceAccountData)
	if yamlToJSONErr != nil {
		status.Message = "Create service account error: yaml to JSON error"
		return status, yamlToJSONErr
	}

	var serviceAccount *corev1.ServiceAccount

	// convert JSON to Service Account type
	jsonUnmarshalErr := json.Unmarshal(serviceAccountJSON, &serviceAccount)
	if jsonUnmarshalErr != nil {
		status.Message = "Create service account error: unmarshal JSON error"
		return status, jsonUnmarshalErr
	}

	serviceAccount.Name = "deploy-service-account-" + nameBase

	dockerSecret := corev1.LocalObjectReference{
		Name: "docker-user-pass-" + nameBase,
	}

	// add secrets to service account
	// serviceAccount.Secrets = append(serviceAccount.Secrets, dockerSecret)
	serviceAccount.ImagePullSecrets = append(serviceAccount.ImagePullSecrets, dockerSecret)

	// set labels
	serviceAccount.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	serviceAccount.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	serviceAccount.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	serviceAccount.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)

	// create service account on cluster
	_, createServiceAccountErr := clientset.CoreV1().ServiceAccounts(namespace).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	if createServiceAccountErr != nil {
		status.Message = "Create service account error"
		return status, createServiceAccountErr
	}

	status.Message = "Create service account succeeded"

	return status, nil
}

func DeleteServiceAccount(clientset *kubernetes.Clientset, namespace string, nameBase string) (Status, error) {
	status := Status{Message: ""}

	serviceAccountName := "deploy-service-account-" + nameBase

	// delete service account on cluster
	deleteServiceAccountErr := clientset.CoreV1().ServiceAccounts(namespace).Delete(context.TODO(), serviceAccountName, metav1.DeleteOptions{})
	if deleteServiceAccountErr != nil {
		status.Message = "Service account deletion error"
		return status, deleteServiceAccountErr
	}

	status.Message = "Service account deletion succeeded"

	return status, nil
}

func CreateDeployURL(namespace, orgName, projectName, serviceName, istioGatewayIP string) (Status, string, error) {
	status := Status{Message: ""}
	// create a hashed dome.tools domain url
	hashing := sha1.New()
	hashing.Write([]byte(orgName + namespace + projectName + serviceName))
	urlHash := hex.EncodeToString(hashing.Sum(nil))
	hostedZone := "dome-tools"
	serviceUrl := urlHash + ".dome.tools."
	dnsRecordType := "A"

	// create a dns service to manipulate the google cloud dns records
	ctx := context.Background()
	dnsService, err := dns.NewService(ctx)
	fmt.Printf("Trying to create domain %s\n", serviceUrl)
	if err != nil {
		status = Status{Message: "error creating a dns service"}
		return status, "", err
	}
	if net.ParseIP(istioGatewayIP) == nil {
		if _, err := url.Parse(istioGatewayIP); err != nil {
			status = Status{Message: "error parsing the istio gateway load balancer endpoint"}
			return status, "", err
		}
		dnsRecordType = "CNAME"
		istioGatewayIP = istioGatewayIP + "."
	}

	rec := &dns.ResourceRecordSet{
		Name:    serviceUrl,
		Rrdatas: []string{istioGatewayIP},
		Ttl:     int64(60),
		Type:    dnsRecordType,
	}

	change := &dns.Change{
		Additions: []*dns.ResourceRecordSet{rec},
	}

	// look for existing records
	list, err := dnsService.ResourceRecordSets.List("dome-engineering", hostedZone).Name(serviceUrl).Do()
	if err != nil {
		status = Status{Message: "error looking up existing records in the dome.tools zone"}
		return status, "", err
	}
	if len(list.Rrsets) > 0 {
		// Attempt to delete the existing records when adding our new one.
		change.Deletions = list.Rrsets
	}
	// create the dns record
	_, err = dnsService.Changes.Create("dome-engineering", hostedZone, change).Do()
	if err != nil {
		status = Status{Message: "error creating a change for a new record in the dome.tools zone"}
		return status, "", err
	}
	status.Message = "Deploy URL dns records succesfully created."
	return status, strings.TrimSuffix(serviceUrl, "."), nil
}

func ExposeDeployment(namespace, appname, internalHostName, customerName, orgName, projectName, serviceName, serviceURL, istioGatewayIP string, customerNetworking []Networking, istioclientset *versionedclient.Clientset) (Status, <-chan watch.Event, error) {
	status := Status{Message: ""}
	destinationHost := appname
	hosts := []string{serviceURL}
	gateways := []string{"istio-system/main-gateway"}
	counter := 0
	vsSpec := &v1alpha3.VirtualService{
		Hosts:    hosts,
		Gateways: gateways,
	}

	for _, n := range customerNetworking {
		if n.Exposed {
			if strings.ToLower(n.Protocol) == "tcp" {
				if internalHostName != "" {
					destinationHost = internalHostName
				}
				vsSpec.Tcp = append(vsSpec.Tcp, &v1alpha3.TCPRoute{
					Match: []*v1alpha3.L4MatchAttributes{
						{
							Port: uint32(n.ExposedPort),
						},
					},
					Route: []*v1alpha3.RouteDestination{
						{
							Destination: &v1alpha3.Destination{
								Host: destinationHost,
								Port: &v1alpha3.PortSelector{
									Number: uint32(n.Port),
								},
							},
						},
					},
				})
			} else {
				vsSpec.Http = append(vsSpec.Http, &v1alpha3.HTTPRoute{
					Match: []*v1alpha3.HTTPMatchRequest{
						{
							Port: uint32(n.ExposedPort),
							Uri: &v1alpha3.StringMatch{
								MatchType: &v1alpha3.StringMatch_Prefix{
									Prefix: "/",
								},
							},
						},
					},
					Rewrite: &v1alpha3.HTTPRewrite{
						Uri: "/",
					},
					Route: []*v1alpha3.HTTPRouteDestination{
						{
							Destination: &v1alpha3.Destination{
								Host: destinationHost,
								Port: &v1alpha3.PortSelector{
									Number: uint32(n.Port),
								},
							},
						},
					},
					Headers: &v1alpha3.Headers{
						Response: &v1alpha3.Headers_HeaderOperations{
							Set: map[string]string{"Strict-Transport-Security": "max-age=31536000"},
							Remove: []string{
								"x-envoy-upstream-healthchecked-cluster",
								"x-envoy-upstream-service-time",
								"Server",
								"Server",
							},
						},
					},
					CorsPolicy: &v1alpha3.CorsPolicy{
						AllowOrigins: []*v1alpha3.StringMatch{
							{
								MatchType: &v1alpha3.StringMatch_Regex{
									Regex: ".*",
								},
							},
						},
						AllowMethods: []string{
							"GET",
							"POST",
							"PUT",
							"DELETE",
							"PATCH",
							"HEAD",
							"OPTIONS",
						},
						AllowHeaders: []string{
							"*",
						},
					},
				})
			}
			counter++
		}
	}
	if counter == 0 {
		// no ports to expose
		fmt.Printf("No external ports selected - an empty client/dummy channel will be returned to be watched\n")
	}

	virtualService := &networkingv1alpha3.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appname + "-virtualservice",
			Namespace: namespace,
			Labels: map[string]string{
				"domenetwork.io/appName":      appname,
				"domenetwork.io/customer":     customerName,
				"domenetwork.io/organization": orgName,
				"domenetwork.io/project":      projectName,
				"domenetwork.io/service":      serviceName,
			},
			Annotations: map[string]string{
				"deployTime": time.Now().String(),
			},
		},
		Spec: *vsSpec,
	}

	virtualServiceClient := istioclientset.NetworkingV1alpha3().VirtualServices(namespace)

	if counter > 0 {
		// Create Istio Virtual Service
		fmt.Println("Creating service...")
		vsResult, vsErr := virtualServiceClient.Create(context.TODO(), virtualService, metav1.CreateOptions{})
		if vsErr != nil {
			status = Status{Message: "error creating an istio virtual service client"}
			return status, nil, vsErr
		}

		fmt.Printf("Created istio virtual service %q.\n", vsResult.GetObjectMeta().GetName())
	}

	watchTimeout := int64(300)
	watchLabel := map[string]string{
		"domenetwork.io/appName": appname,
	}

	virtualServiceWatchInterface, virtualServiceWatchChannelErr := virtualServiceClient.Watch(context.TODO(), metav1.ListOptions{
		LabelSelector:  labels.Set(watchLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})

	if virtualServiceWatchChannelErr != nil {
		return status, nil, virtualServiceWatchChannelErr
	}
	virtualServiceWatchChannel := virtualServiceWatchInterface.ResultChan()

	return status, virtualServiceWatchChannel, nil

}

// utility function to remove spaces and lowercase k8 object labels
func formatLabel(labelInput string) string {
	label := strings.ToLower(strings.Replace(labelInput, " ", "-", -1))
	return label
}

func readYamlFile(filepath string) ([]byte, error) {
	// load the file into a buffer
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func validProtocol(protocol string) bool {
	if protocol == string(corev1.ProtocolTCP) || protocol == string(corev1.ProtocolUDP) || protocol == string(corev1.ProtocolSCTP) {
		return true
	} else {
		return protocol == "HTTP"
	}
}

func Init(kubeConfigPath string) (*kubernetes.Clientset, *restclient.Config, *versionedclient.Clientset, Status, error) {
	// NewFlagSet to avoid flag redefined error
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// set Kubernetes config
	var kubeconfig *string
	status := Status{Message: ""}

	kubeconfig = flag.String("kubeconfig", kubeConfigPath, "absolute path to the kubeconfig file")

	flag.Parse()

	config, configErr := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if configErr != nil {
		status.Message = "Init error occured"
		return nil, nil, nil, status, configErr
	}

	// create default Kubernetes clientset
	clientset, clientsetErr := kubernetes.NewForConfig(config)
	if clientsetErr != nil {
		status.Message = "Init error occured for the default k8s cilent"
		return nil, nil, nil, status, errNotExist()
	}

	// create the istio clientset
	istioclientset, err := versionedclient.NewForConfig(config)
	if err != nil {
		status.Message = "Init error occured for the istio client"
		return nil, nil, nil, status, errNotExist()
	}

	status.Message = "Init succeeded"

	return clientset, config, istioclientset, status, nil
}
