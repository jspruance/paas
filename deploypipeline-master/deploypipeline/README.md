# Dome Deploy Pipeline

## Update `deploypipeline` Instructions

1. Create new deploypipeline release/tag with format `vX.X.X`
2. `export GOPRIVATE=git.domenetwork.io`
3. add to `.gitconfig`:

```
[url "ssh://git@git.domenetwork.io/"]
	insteadOf = https://git.domenetwork.io/
```

4. `go get -u git.domenetwork.io/dome/deploypipeline`
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

	"git.domenetwork.io/dome/deploypipeline"
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

	// create customer config object for deploy pipeline
	customerConfig := deploypipeline.CustomerData{
		AppName:             "ocean-api",
		CustomerName:        "Dome",
		CustomerProjectName: "Master Blaster",
		CustomerServiceName: "Build Pipeline",
		DockerImageName:     "jspruancedome/ocean-api:dome",
		Namespace:           "test-pipeline",
		SSHPrivateKey:       secretData,
	}

	// initialize deploy pipeline
	deployClusterPath := "kubeconfig.yaml"
	clientset, config, initStatus, initErr := deploypipeline.Init(deployClusterPath)
	if initErr != nil {
		fmt.Println(initErr)
	}

	fmt.Println(clientset)
	fmt.Println(config)
	fmt.Println(initStatus.Message)

	deployPipelineStatus, deployPipelineErr := deploypipeline.CreateDeployment(customerConfig, clientset)
	if deployPipelineErr != nil {
		fmt.Println(deployPipelineErr)
	}

	fmt.Println(deployPipelineStatus)

	// scale deployment down to 0 ReplicaSets
	scaleDownResult, scaleDownErr := deploypipeline.ScaleDeployment(customerConfig, clientset, 0)
	if scaleDownErr != nil {
		fmt.Println(scaleDownErr)
	}
	fmt.Println(scaleDownResult)

	// scale deployment up to 2 ReplicaSets
	scaleUpResult, scaleUpErr := deploypipeline.ScaleDeployment(customerConfig, clientset, 2)
	if scaleUpErr != nil {
		fmt.Println(scaleUpErr)
	}
	fmt.Println(scaleUpResult)

	// kubectl -n test-pipeline describe deployment ocean-api
}
```