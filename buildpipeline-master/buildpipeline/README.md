# Dome Build Pipeline

## Update `buildpipeline` Instructions

1. Create new buildpipeline release/tag with format `vX.X.X`
2. `export GOPRIVATE=git.domenetwork.io`
3. add to `.gitconfig`:

```
[url "ssh://git@git.domenetwork.io/"]
	insteadOf = https://git.domenetwork.io/
```

4. `go get -u git.domenetwork.io/dome/buildpipeline`
5. `go mod tidy`
6. `go mod vendor`
7. `commit all changes, open and merge PR`

## Sample client implementation

```
package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"runtime"

	"git.domenetwork.io/dome/buildpipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"k8s.io/client-go/util/homedir"
)

func main() {
	// get the file containing ssh private key
	var (
		_, b, _, _ = runtime.Caller(0)
		basepath   = filepath.Dir(b)
	)

	fname := filepath.Join(basepath, "id_rsa")

	// load the private key file into a buffer
	secretData, err := ioutil.ReadFile(fname)
	if err != nil {
		log.Fatal(err)
	}

	// create customer config object for build pipeline
	customerConfig := buildpipeline.CustomerData{
		CustomerName:             "Dome",
		CustomerOrganizationName: "Dome Engineering",
		CustomerProjectName:      "Master Blaster",
		CustomerServiceName:      "Build Pipeline",
		DockerImageName:          "jspruancedome/ocean-api:dome",
		GitRevision:              "", // empty for latest commit hash
		Namespace:                "test-pipeline",
		RepositoryUrl:            "git@github.com:jspruance/ocean-api",
		SSHPrivateKey:            secretData,
		DockerRegistrySecretPath: "docker-registry-secret.yaml",
		PipelinePath:             "pipeline.yaml",
		PipelineRunPath:          "pipeline-run.yaml",
		ServiceAccountPath:       "service-account.yaml",
		ResourcesPath:            "resources.yaml",
	}

	// 1) initialize build pipeline
	buildClusterPath := filepath.Join(homedir.HomeDir(), "kubeconfig.yaml")

	clientset, config, initStatus, initErr := buildpipeline.Init(buildClusterPath)
	if initErr != nil {
		fmt.Println(initErr)
	}

	fmt.Println(initStatus.Message)

	// 2) create client namespace
	_, _, namespaceErr := buildpipeline.CreateNamespace(customerConfig.Namespace, clientset)
	if namespaceErr != nil {
		fmt.Println(namespaceErr)
	}

	// 3) create build pipeline, specific to customer
	createPipelineStatus, buildPipelineWatchChannel, createPipelineErr := buildpipeline.CreateBuildPipeline(customerConfig, clientset, config)
	if createPipelineErr != nil {
		fmt.Println(createPipelineErr)
	}

	fmt.Println(createPipelineStatus.Message)

	// Get pipeline status in real-time from watch events piped through channel
	// Exit when 'Success' type resolving to 'True' status returned
	done := make(chan bool)
	go func(done chan bool) {
		for event := range buildPipelineWatchChannel {
			fmt.Printf("Pipeline watch event type: %v\n", event.Type)
			p, ok := event.Object.(*v1beta1.PipelineRun)
			if !ok {
				log.Fatal("Unexpected pipeline event type")
			}
			fmt.Println("** Current pipeline status **")
			if len(p.Status.Status.Conditions) > 0 {
				fmt.Println(p.Status.Status.Conditions[0].Message) // Tasks Completed: 3 (Failed: 0, Cancelled 0), Skipped: 0
				fmt.Println(p.Status.Status.Conditions[0].Reason)  // Succeeded
				fmt.Println(p.Status.Status.Conditions[0].Status)  // True
				fmt.Println(p.Status.Status.Conditions[0].Type)    // Succeeded

				if p.Status.Status.Conditions[0].Type == "Succeeded" && p.Status.Status.Conditions[0].Status == "True" {
					done <- true
				}
			}
		}
	}(done)
	<-done

	fmt.Println("Deleting build pipeline")

	_, deleteBuildPipelienErr := buildpipeline.DeleteBuildPipeline(clientset, customerConfig, config)
	if deleteBuildPipelienErr != nil {
		fmt.Println(deleteBuildPipelienErr)
	}

	fmt.Println("Build pipeline deleted")
}
```