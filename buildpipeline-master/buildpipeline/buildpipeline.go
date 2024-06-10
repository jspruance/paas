// name of package
package buildpipeline

import (
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	v1beta1ClientTypes "github.com/tektoncd/pipeline/pkg/client/clientset/versioned/typed/pipeline/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Status struct {
	Message string
}

type CustomerData struct {
	AppName                        string
	BuildClusterPath               string
	CustomerName                   string
	CustomerOrganizationName       string
	CustomerProjectName            string
	CustomerServiceName            string
	DockerImageName                string
	RegistryUrl                    string
	GitRevision                    string
	Namespace                      string
	RepositoryUrl                  string
	SSHPrivateKey                  []byte
	DockerRegistrySecretPath       string
	DockerKanikoRegistrySecretPath string
	PipelinePath                   string
	PipelineRunPath                string
	ServiceAccountPath             string
	ResourcesPath                  string
	BuildPipelineID                string
	BuildPipelineInitForService    bool
	BuildFromDockerfile            string
	Builder                        string
}

// clients holds instances of interfaces for making requests to the Pipeline controllers.
type clients struct {
	V1beta1PipelineClient    v1beta1ClientTypes.PipelineInterface
	V1beta1ClusterTaskClient v1beta1ClientTypes.ClusterTaskInterface
	V1beta1TaskClient        v1beta1ClientTypes.TaskInterface
	V1beta1TaskRunClient     v1beta1ClientTypes.TaskRunInterface
	V1beta1PipelineRunClient v1beta1ClientTypes.PipelineRunInterface
}

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

func getClientSets(config *rest.Config, namespace string) (*clients, Status, error) {
	c := &clients{}
	status := Status{Message: ""}

	tektonClientSet, err := versioned.NewForConfig(config)
	if err != nil {
		status.Message = "Client set retrieval error"
		return nil, status, errTektonClientSet()
	}

	c.V1beta1PipelineClient = tektonClientSet.TektonV1beta1().Pipelines(namespace)
	c.V1beta1ClusterTaskClient = tektonClientSet.TektonV1beta1().ClusterTasks()
	c.V1beta1TaskClient = tektonClientSet.TektonV1beta1().Tasks(namespace)
	c.V1beta1TaskRunClient = tektonClientSet.TektonV1beta1().TaskRuns(namespace)
	c.V1beta1PipelineRunClient = tektonClientSet.TektonV1beta1().PipelineRuns(namespace)

	status.Message = "Client set retrieval succeeded"

	return c, status, nil
}

// MustParseV1beta1Task takes YAML and parses it into a *v1beta1.Task
// borrowed and modified from https://github.com/tektoncd/pipeline/blob/main/test/parse/yaml.go
func mustParseV1beta1Task(yaml string) *v1beta1.Task {
	var task v1beta1.Task
	yaml = `apiVersion: tekton.dev/v1beta1
kind: Task
` + yaml
	mustParseYAML(yaml, &task)
	return &task
}

// MustParseV1beta1Pipeline takes YAML and parses it into a *v1beta1.Pipeline
// borrowed and modified from https://github.com/tektoncd/pipeline/blob/main/test/parse/yaml.go
func mustParseV1beta1Pipeline(yaml string) *v1beta1.Pipeline {
	var pipeline v1beta1.Pipeline
	yaml = `apiVersion: tekton.dev/v1beta1
kind: Pipeline
` + yaml
	mustParseYAML(yaml, &pipeline)
	return &pipeline
}

// MustParseV1beta1PipelineRun takes YAML and parses it into a *v1beta1.PipelineRun
// borrowed and modified from https://github.com/tektoncd/pipeline/blob/main/test/parse/yaml.go
func mustParseV1beta1PipelineRun(yaml string) *v1beta1.PipelineRun {
	var pr v1beta1.PipelineRun
	yaml = `apiVersion: tekton.dev/v1beta1
kind: PipelineRun
` + yaml
	mustParseYAML(yaml, &pr)
	return &pr
}

// borrowed and modified from https://github.com/tektoncd/pipeline/blob/main/test/parse/yaml.go
func mustParseYAML(yaml string, i k8runtime.Object) {
	if _, _, err := scheme.Codecs.UniversalDeserializer().Decode([]byte(yaml), nil, i); err != nil {
		log.Err(err).Msgf("mustParseYAML (%s): %v", yaml)
	}
}

// utility function to remove spaces and lowercase k8 object labels
func formatLabel(labelInput string) string {
	label := strings.ToLower(strings.Replace(labelInput, " ", "-", -1))
	return label
}

func fetchResource(url string) (string, Status, error) {
	status := Status{Message: ""}

	resp, err := http.Get(url)

	if err != nil {
		status.Message = "Error fetching resource"
		return "", status, err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		status.Message = "Error reading response of fetched resource"
		return "", status, err
	}

	status.Message = "Fetch succeeded"

	return string(body), status, nil
}

func readYamlFile(filepath string) ([]byte, error) {
	// load the file into a buffer
	data, err := ioutil.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func CreateTask(clientSets *clients, task *v1beta1.Task) (Status, error) {
	ctx := context.Background()
	status := Status{Message: ""}

	if _, createTaskErr := clientSets.V1beta1TaskClient.Create(ctx, task, metav1.CreateOptions{}); createTaskErr != nil {
		status.Message = "Create task error"
		return status, createTaskErr
	}

	status.Message = "Create task succeeded"

	return status, nil
}

func DeleteTask(clientSets *clients, taskName string) (Status, error) {
	ctx := context.Background()
	status := Status{Message: ""}

	deleteTaskErr := clientSets.V1beta1TaskClient.Delete(ctx, taskName, metav1.DeleteOptions{})
	if deleteTaskErr != nil {
		status.Message = "Delete task error"
		return status, deleteTaskErr
	}

	status.Message = "Task successfully deleted"

	return status, nil
}

func CreateDockerRegistrySecret(clientset *kubernetes.Clientset, namespace string, dockerRegistrySecretPath string, customer CustomerData, pipelineNameBase string) (Status, error) {
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
	secret.Name = "docker-user-pass"

	// set labels
	secret.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	secret.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	secret.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	secret.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	secret.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

	// create secret on cluster
	_, createDockerSecret := clientset.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if createDockerSecret != nil {
		status.Message = "Create Docker registry secret error"
		return status, createDockerSecret
	}

	status.Message = "Create Docker registry secret succeeded"

	return status, nil
}

func CreateKanikoDockerRegistrySecret(clientset *kubernetes.Clientset, namespace string, dockerKanikoRegistrySecretPath string, customer CustomerData, pipelineNameBase string) (Status, error) {
	status := Status{Message: ""}

	// load the file into a buffer
	secretData, err := ioutil.ReadFile(dockerKanikoRegistrySecretPath)
	if err != nil {
		status.Message = "Create Kaniko Docker registry secret error: read file"
		return status, err
	}

	// convert yaml to JSON
	secretJSON, yamlToJSONErr := yaml.ToJSON(secretData)
	if yamlToJSONErr != nil {
		status.Message = "Create Kaniko Docker registry secret error: convert yaml to JSON"
		return status, yamlToJSONErr
	}

	var secret *corev1.Secret

	// convert JSON to Secret type
	jsonUnmarshalErr := json.Unmarshal(secretJSON, &secret)
	if jsonUnmarshalErr != nil {
		status.Message = "Create Kaniko Docker registry secret error: unmarshal"
		return status, jsonUnmarshalErr
	}

	secret.Namespace = namespace

	// set labels
	secret.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	secret.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	secret.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	secret.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	secret.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

	// create secret on cluster
	_, createDockerSecret := clientset.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if createDockerSecret != nil {
		status.Message = "Create Kaniko Docker registry secret error"
		return status, createDockerSecret
	}

	status.Message = "Create Kaniko Docker registry secret succeeded"

	return status, nil
}

func DeleteDockerRegistrySecret(clientset *kubernetes.Clientset, namespace string, pipelineNameBase string) (Status, error) {
	status := Status{Message: ""}

	secretName := "docker-user-pass"

	// delete secret on cluster
	deleteDockerSecret := clientset.CoreV1().Secrets(namespace).Delete(context.TODO(), secretName, metav1.DeleteOptions{})
	if deleteDockerSecret != nil {
		status.Message = "Docker registry secret deletion error"
		return status, deleteDockerSecret
	}

	status.Message = "Docker registry secret succeesfully deleted"

	return status, nil
}

func CreateGitSecret(clientset *kubernetes.Clientset, namespace string, sshKey []byte, nameBase string) (Status, error) {
	status := Status{Message: ""}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ssh-key-" + nameBase,
			Annotations: map[string]string{"tekton.dev/git-0": "github.com"},
		},
		Type: "kubernetes.io/ssh-auth",
		Data: map[string][]byte{"ssh-privatekey": sshKey},
	}

	// create secret on cluster
	_, createSecretErr := clientset.CoreV1().Secrets(namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
	if createSecretErr != nil {
		status.Message = "Create Git secret error"
		return status, createSecretErr
	}

	status.Message = "Create Git secret succeeded"

	return status, nil
}

func DeleteGitSecret(clientset *kubernetes.Clientset, namespace string, nameBase string) (Status, error) {
	status := Status{Message: ""}

	secretName := "ssh-key-" + nameBase

	// delete secret on cluster
	deleteSecretErr := clientset.CoreV1().Secrets(namespace).Delete(context.TODO(), secretName, metav1.DeleteOptions{})
	if deleteSecretErr != nil {
		status.Message = "Delete Git secret error"
		return status, deleteSecretErr
	}

	status.Message = "Git secret succeesfully deleted"

	return status, nil
}

func CreatePersistentVolumeClaim(clientset *kubernetes.Clientset, namespace string, resourcesPath string, customer CustomerData) (Status, error) {
	status := Status{Message: ""}
	persistentVolumeClaimData, readYamlError := readYamlFile(resourcesPath) // resources.yaml

	if readYamlError != nil {
		status.Message = "Create PVC error: read yaml error"
		return status, readYamlError
	}

	// convert yaml to JSON
	persistentVolumeClaimJSON, yamlToJSONErr := yaml.ToJSON(persistentVolumeClaimData)
	if yamlToJSONErr != nil {
		status.Message = "Create PVC error: yaml to JSON error"
		return status, yamlToJSONErr
	}

	var persistentVolumeClaim *corev1.PersistentVolumeClaim

	// convert JSON to Service Account type
	jsonUnmarshalErr := json.Unmarshal(persistentVolumeClaimJSON, &persistentVolumeClaim)
	if jsonUnmarshalErr != nil {
		status.Message = "Create PVC error: unmarshal error"
		return status, jsonUnmarshalErr
	}

	// set labels
	persistentVolumeClaim.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	persistentVolumeClaim.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	persistentVolumeClaim.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	persistentVolumeClaim.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	persistentVolumeClaim.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

	existingPVC, _ := clientset.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), persistentVolumeClaim.Name, metav1.GetOptions{})

	if existingPVC.Name != persistentVolumeClaim.Name {
		// create persistent volume claim on cluster
		_, createPersistentVolumeClaimErr := clientset.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), persistentVolumeClaim, metav1.CreateOptions{})
		if createPersistentVolumeClaimErr != nil {
			status.Message = "Create PVC error"
			return status, createPersistentVolumeClaimErr
		}

		status.Message = "Create PVC succeeded"
		return status, nil
	}

	status.Message = "Existing PVC"

	return status, nil
}

func DeletePersistentVolumeClaim(clientset *kubernetes.Clientset, namespace string) (Status, error) {
	status := Status{Message: ""}

	persistentVolumeClaimName := "buildpacks-source-pvc"

	// delete persistent volume claim on cluster
	deletePersistentVolumeClaimErr := clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(context.TODO(), persistentVolumeClaimName, metav1.DeleteOptions{})
	if deletePersistentVolumeClaimErr != nil {
		status.Message = "Error occured while deleting persistent volume claim"
		return status, deletePersistentVolumeClaimErr
	}

	status.Message = "Persistent volume claim succeesfully deleted"

	return status, nil
}

func CreateServiceAccount(clientset *kubernetes.Clientset, namespace string, serviceAccountPath string, customer CustomerData, nameBase string, pipelineNameBase string) (Status, error) {
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

	serviceAccount.Name = "buildpacks-service-account" + nameBase

	gitSecret := corev1.ObjectReference{
		Name: "ssh-key-" + nameBase,
	}

	dockerSecret := corev1.ObjectReference{
		Name: "docker-user-pass",
	}

	// add secrets to service account
	serviceAccount.Secrets = append(serviceAccount.Secrets, gitSecret)
	serviceAccount.Secrets = append(serviceAccount.Secrets, dockerSecret)

	// set labels
	serviceAccount.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	serviceAccount.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	serviceAccount.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	serviceAccount.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	serviceAccount.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

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

	serviceAccountName := "buildpacks-service-account" + nameBase

	// delete service account on cluster
	deleteServiceAccountErr := clientset.CoreV1().ServiceAccounts(namespace).Delete(context.TODO(), serviceAccountName, metav1.DeleteOptions{})
	if deleteServiceAccountErr != nil {
		status.Message = "Service account deletion error"
		return status, deleteServiceAccountErr
	}

	status.Message = "Service account deletion succeeded"

	return status, nil
}

func CreatePipeline(clientSets *clients, namespace string, pipelineNameBase string, pipelinePath string, customer CustomerData) (Status, error) {
	pipelineData, readYamlError := readYamlFile(pipelinePath) // pipeline.yaml
	status := Status{Message: ""}

	if readYamlError != nil {
		status.Message = "Create pipeline error: read yaml error"
		return status, readYamlError
	}

	pipeline := mustParseV1beta1Pipeline(string(pipelineData))

	pipeline.Namespace = namespace
	pipeline.Name = pipelineNameBase + "-pipeline"

	// set builder, if provided
	if customer.Builder != "" {
		for i, task := range pipeline.Spec.Tasks {
			if task.Name == "buildpacks" {
				for j, param := range task.Params {
					newBuilderValue := v1beta1.ParamValue{
						Type:      "string",
						StringVal: customer.Builder,
					}
					if param.Name == "BUILDER_IMAGE" {
						pipeline.Spec.Tasks[i].Params[j].Value = newBuilderValue
					}
				}
			}
		}
	}

	// set labels
	pipeline.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	pipeline.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	pipeline.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	pipeline.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	pipeline.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

	// create pipeline on cluster
	ctx := context.Background()

	if _, createPipelineErr := clientSets.V1beta1PipelineClient.Create(ctx, pipeline, metav1.CreateOptions{}); createPipelineErr != nil {
		status.Message = "Create pipeline error"
		return status, createPipelineErr
	}

	status.Message = "Create pipeline succeeded"

	return status, nil
}

func DeletePipeline(clientSets *clients, namespace string, pipelineNameBase string) (Status, error) {
	status := Status{Message: ""}

	pipelineName := pipelineNameBase + "-pipeline"
	ctx := context.Background()

	deletePipelineErr := clientSets.V1beta1PipelineClient.Delete(ctx, pipelineName, metav1.DeleteOptions{})
	if deletePipelineErr != nil {
		status.Message = "Delete pipeline error"
		return status, deletePipelineErr
	}

	return status, nil
}

func CreatePipelineRun(clientSets *clients, customer CustomerData, pipelineNameBase string, pipelineRunPath string, nameBase string) (Status, error) {
	pipelineRunData, readYamlError := readYamlFile(pipelineRunPath) // pipeline-run.yaml
	status := Status{Message: ""}

	if readYamlError != nil {
		status.Message = "Create pipeline run error: read yaml error"
		return status, readYamlError
	}

	pipelineRun := mustParseV1beta1PipelineRun(string(pipelineRunData))

	pipelineRun.GenerateName = pipelineNameBase + "-pipeline-run-"
	pipelineRun.Namespace = customer.Namespace
	pipelineRun.Spec.PipelineRef.Name = pipelineNameBase + "-pipeline"

	pipelineRun.Spec.ServiceAccountName = "buildpacks-service-account" + nameBase

	// set labels
	pipelineRun.Labels["domenetwork.io/organization"] = formatLabel(customer.CustomerOrganizationName)
	pipelineRun.Labels["domenetwork.io/project"] = formatLabel(customer.CustomerProjectName)
	pipelineRun.Labels["domenetwork.io/service"] = formatLabel(customer.CustomerServiceName)
	pipelineRun.Labels["domenetwork.io/appName"] = formatLabel(customer.AppName)
	pipelineRun.Labels["domenetwork.io/buildpipelineID"] = formatLabel(customer.BuildPipelineID)

	for i, value := range pipelineRun.Spec.Params {
		newImageValue := v1beta1.ParamValue{
			Type:      "string",
			StringVal: customer.RegistryUrl + "/" + customer.DockerImageName,
		}

		newUrlValue := v1beta1.ParamValue{
			Type:      "string",
			StringVal: customer.RepositoryUrl,
		}

		newRevisionValue := v1beta1.ParamValue{
			Type:      "string",
			StringVal: customer.GitRevision,
		}

		newFromDockerfile := v1beta1.ParamValue{
			Type:      "string",
			StringVal: customer.BuildFromDockerfile,
		}

		if value.Name == "image" {
			pipelineRun.Spec.Params[i].Value = newImageValue
		}

		if value.Name == "url" {
			pipelineRun.Spec.Params[i].Value = newUrlValue
		}

		if value.Name == "revision" {
			pipelineRun.Spec.Params[i].Value = newRevisionValue
		}

		if value.Name == "fromDockerfile" {
			pipelineRun.Spec.Params[i].Value = newFromDockerfile
		}
	}

	// create pipeline run on cluster
	ctx := context.Background()

	if _, createPipelineRunErr := clientSets.V1beta1PipelineRunClient.Create(ctx, pipelineRun, metav1.CreateOptions{}); createPipelineRunErr != nil {
		status.Message = "Create pipeline run error"
		return status, createPipelineRunErr
	}

	status.Message = "Create pipeline run succeeded"

	return status, nil
}

func DeletePipelineRun(clientSets *clients, namespace string, pipelineNameBase string) (Status, error) {
	status := Status{Message: ""}

	pipelineRunNameBase := pipelineNameBase + "-pipeline-run-"
	pipelineRunName := ""

	ctx := context.Background()

	pipelineRunList, pipelineRunListErr := clientSets.V1beta1PipelineRunClient.List(ctx, metav1.ListOptions{})
	if pipelineRunListErr != nil {
		return status, pipelineRunListErr
	}

	for _, value := range pipelineRunList.Items {
		if strings.Contains(value.Name, pipelineRunNameBase) {
			pipelineRunName = value.Name
			break
		}
	}

	deletePipelineRunErr := clientSets.V1beta1PipelineRunClient.Delete(ctx, pipelineRunName, metav1.DeleteOptions{})
	if deletePipelineRunErr != nil {
		status.Message = "Delete pipeline error"
		return status, deletePipelineRunErr
	}

	return status, nil
}

func watchPipelineRun(namespace string, config *restclient.Config, buildPipelineID string) (<-chan watch.Event, error) {
	ctx := context.Background()

	// get clients object
	clientSets, _, errGetClientSets := getClientSets(config, namespace)
	if errGetClientSets != nil {
		return nil, errGetClientSets
	}

	watchTimeout := int64(1800)
	buildPipelineIDLabel := map[string]string{
		"domenetwork.io/buildpipelineID": buildPipelineID,
	}

	watchInterface, watchErr := clientSets.V1beta1PipelineRunClient.Watch(ctx, metav1.ListOptions{
		LabelSelector:  labels.Set(buildPipelineIDLabel).String(),
		TimeoutSeconds: &watchTimeout,
	})
	if watchErr != nil {
		return nil, watchErr
	}

	watchChan := watchInterface.ResultChan()

	return watchChan, nil
}

func CreateBuildPipeline(customer CustomerData, clientset *kubernetes.Clientset, config *restclient.Config) (Status, <-chan watch.Event, error) {
	// capture & format input args
	appName := customer.AppName
	buildPipelineID := customer.BuildPipelineID
	namespace := customer.Namespace
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	pipelineRunPath := customer.PipelineRunPath
	nameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName+"-"+appName), " ", "-", -1)
	pipelineNameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName), " ", "-", -1)
	initialBuildPipeline := true
	initForService := customer.BuildPipelineInitForService

	status := Status{Message: ""}

	// check to see if the specified namespace exists
	currentNamespace, _ := GetNamespace(namespace, clientset)

	if currentNamespace != nil && len(currentNamespace.Name) > 0 && currentNamespace.Status.Phase != "Terminating" {
		initialBuildPipeline = false
	} else {
		// create namespace if it doesn't yet exist
		_, createNamespaceStatus, createNamespaceErr := CreateNamespace(namespace, clientset)
		if createNamespaceErr != nil {
			return createNamespaceStatus, nil, createNamespaceErr
		}
		log.Info().Msgf(createNamespaceStatus.Message)
		initialBuildPipeline = true
	}

	// initialize build pipeline for service
	if !initForService {
		serviceBuildPipelineInitStatus, serviceBuildPipelineInitError := ServiceBuildPipelineInit(customer, clientset, config)
		if serviceBuildPipelineInitError != nil {
			status.Message = "Error initializing build pipeline for service"
			return status, nil, serviceBuildPipelineInitError
		}
		log.Info().Msgf(serviceBuildPipelineInitStatus.Message)
	}

	// get clients object
	clientSets, getClientSetsStatus, errGetClientSets := getClientSets(config, namespace)
	if errGetClientSets != nil {
		return getClientSetsStatus, nil, errTektonClientSet()
	}

	// initialize build pipeline for namespace
	if initialBuildPipeline {
		namespaceBuildPipelineInitStatus, namespaceBuildPipelineInitError := NamespaceBuildPipelineInit(customer, clientset, config, clientSets)
		if namespaceBuildPipelineInitError != nil {
			status.Message = "Error initializing build pipeline for namespace"
			return status, nil, namespaceBuildPipelineInitError
		}
		log.Info().Msgf(namespaceBuildPipelineInitStatus.Message)
	}

	// EVERY TIME
	createPipelineStatus, createPipelineRun := CreatePipelineRun(clientSets, customer, pipelineNameBase, pipelineRunPath, nameBase)
	if createPipelineRun != nil {
		return createPipelineStatus, nil, createPipelineRun
	}

	pipelineRunWatchChan, pipelineRunWatchErr := watchPipelineRun(namespace, config, formatLabel(buildPipelineID))
	if pipelineRunWatchErr != nil {
		return status, nil, pipelineRunWatchErr
	}

	status.Message = "Build pipeline running"

	return status, pipelineRunWatchChan, nil
}

func DeleteBuildPipeline(clientset *kubernetes.Clientset, customer CustomerData, config *restclient.Config) (Status, error) {
	status := Status{Message: ""}

	// capture & format input args
	namespace := customer.Namespace
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	pipelineNameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName), " ", "-", -1)

	// get clients object
	clientSets, getClientSetsStatus, errGetClientSets := getClientSets(config, namespace)
	if errGetClientSets != nil {
		return getClientSetsStatus, errGetClientSets
	}

	// delete pipeline run
	deletePipelineRunStatus, deletePipelineRunErr := DeletePipelineRun(clientSets, namespace, pipelineNameBase)
	if deletePipelineRunErr != nil {
		return deletePipelineRunStatus, deletePipelineRunErr
	}

	status.Message = "Build pipeline successfully deleted"

	return status, nil
}

func CleanBuildPipeline(clientset *kubernetes.Clientset, customer CustomerData, config *restclient.Config) []error {
	var errors []error

	// capture & format input args
	namespace := customer.Namespace
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	pipelineNameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName), " ", "-", -1)

	// get clients object
	clientSets, _, errGetClientSets := getClientSets(config, namespace)
	if errGetClientSets != nil {
		errors = append(errors, errGetClientSets)
		return errors
	}

	// delete pipeline run
	_, deletePipelineRunErr := DeletePipelineRun(clientSets, namespace, pipelineNameBase)
	if deletePipelineRunErr != nil {
		errors = append(errors, deletePipelineRunErr)
	}

	return errors
}

func NamespaceBuildPipelineInit(customer CustomerData, clientset *kubernetes.Clientset, config *restclient.Config, clientSets *clients) (Status, error) {
	// this code runs once per namespace
	namespace := customer.Namespace
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	pipelineNameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName), " ", "-", -1)
	dockerRegistrySecretPath := customer.DockerRegistrySecretPath

	pipelinePath := customer.PipelinePath
	status := Status{Message: ""}

	// create secret for Docker registry auth
	createDockerSecretStatus, createDockerSecretErr := CreateDockerRegistrySecret(clientset, namespace, dockerRegistrySecretPath, customer, pipelineNameBase)
	if createDockerSecretErr != nil {
		return createDockerSecretStatus, createDockerSecretErr
	}

	// create secret for Docker registry auth
	if customer.BuildFromDockerfile == "true" {
		dockerKanikoRegistrySecretPath := customer.DockerKanikoRegistrySecretPath
		createKanikoDockerSecretStatus, createKanikoDockerSecretErr := CreateKanikoDockerRegistrySecret(clientset, namespace, dockerKanikoRegistrySecretPath, customer, pipelineNameBase)
		if createKanikoDockerSecretErr != nil {
			return createKanikoDockerSecretStatus, createKanikoDockerSecretErr
		}
	}

	// create prerequisite tasks
	// fetch tasks from repo
	cloneTaskUrl := "https://raw.githubusercontent.com/tektoncd/catalog/master/task/git-clone/0.4/git-clone.yaml"
	buildpacksTaskUrl := "https://raw.githubusercontent.com/tektoncd/catalog/master/task/buildpacks/0.3/buildpacks.yaml"
	kanikoTaskUrl := "https://api.hub.tekton.dev/v1/resource/tekton/task/kaniko/0.6/raw"

	cloneTaskString, fetchStatus, fetchCloneTaskError := fetchResource(cloneTaskUrl)
	if fetchCloneTaskError != nil {
		return fetchStatus, fetchCloneTaskError
	}

	buildpacksTaskString, fetchStatus, buildpacksTaskErr := fetchResource(buildpacksTaskUrl)
	if buildpacksTaskErr != nil {
		return fetchStatus, buildpacksTaskErr
	}

	kanikoTaskString, fetchStatus, kanikoTaskErr := fetchResource(kanikoTaskUrl)
	if kanikoTaskErr != nil {
		return fetchStatus, kanikoTaskErr
	}

	// replce leading "---" in Buildpacks task so that it can be properly parsed and converted to a Task struct
	buildpacksTaskString = strings.Replace(buildpacksTaskString, "---", "", 1)

	// convert yaml strings to Tekton Task structs
	cloneTask := mustParseV1beta1Task(cloneTaskString)
	buildpacksTask := mustParseV1beta1Task(buildpacksTaskString)
	kanikoTask := mustParseV1beta1Task(kanikoTaskString)

	// create tasks on Kubernetes cluster
	createCloneTaskStatus, createCloneTaskErr := CreateTask(clientSets, cloneTask)
	if createCloneTaskErr != nil {
		return createCloneTaskStatus, createCloneTaskErr
	}

	createBuildpacksTaskStatus, createBuildpacksTaskErr := CreateTask(clientSets, buildpacksTask)
	if createBuildpacksTaskErr != nil {
		return createBuildpacksTaskStatus, createBuildpacksTaskErr
	}

	createKanikoTaskStatus, createKanikoTaskErr := CreateTask(clientSets, kanikoTask)
	if createKanikoTaskErr != nil {
		return createKanikoTaskStatus, createKanikoTaskErr
	}

	// create Tekton pipeline
	createPipelineStatus, createPipelineErr := CreatePipeline(clientSets, namespace, pipelineNameBase, pipelinePath, customer)
	if createPipelineErr != nil {
		return createPipelineStatus, createPipelineErr
	}

	status.Message = "Build pipeline initialized for namespace"

	return status, nil
}

func ServiceBuildPipelineInit(customer CustomerData, clientset *kubernetes.Clientset, config *restclient.Config) (Status, error) {
	// this code runs once per service
	appName := customer.AppName
	namespace := customer.Namespace
	customerName := customer.CustomerName
	customerProjectName := customer.CustomerProjectName
	githubSSHKey := customer.SSHPrivateKey
	serviceAccountPath := customer.ServiceAccountPath
	resourcesPath := customer.ResourcesPath
	nameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName+"-"+appName), " ", "-", -1)
	pipelineNameBase := strings.Replace(strings.ToLower(customerName+"-"+customerProjectName), " ", "-", -1)
	status := Status{Message: ""}

	// create git for GitHub private repos
	createSecretStatus, createSecretErr := CreateGitSecret(clientset, namespace, githubSSHKey, nameBase)
	if createSecretErr != nil {
		return createSecretStatus, createSecretErr
	}

	// create Persistent Volume Claim for use by buildpacks
	createPersistentVolumeClaimStatus, createPersistentVolumeClaimErr := CreatePersistentVolumeClaim(clientset, namespace, resourcesPath, customer)
	if createPersistentVolumeClaimErr != nil {
		return createPersistentVolumeClaimStatus, createPersistentVolumeClaimErr
	}

	// create 'build-packs-service' service account
	createServiceAccountStatus, createServiceAccountErr := CreateServiceAccount(clientset, namespace, serviceAccountPath, customer, nameBase, pipelineNameBase)
	if createServiceAccountErr != nil {
		return createServiceAccountStatus, createServiceAccountErr
	}

	status.Message = "Build pipeline successfully initialized for service"

	return status, nil
}

func Init(kubeConfigPath string) (*kubernetes.Clientset, *restclient.Config, Status, error) {
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
		return nil, nil, status, configErr
	}

	// create default Kubernetes clientset
	clientset, clientsetErr := kubernetes.NewForConfig(config)
	if clientsetErr != nil {
		status.Message = "Init error occured"
		return nil, nil, status, errNotExist()
	}

	status.Message = "Init succeeded"

	return clientset, config, status, nil
}
