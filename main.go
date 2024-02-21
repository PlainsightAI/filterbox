package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v3/pkg/cli"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"github.com/nexidian/gocliselect"
	"github.com/pkg/errors"
	urcli "github.com/urfave/cli/v2"
	"golang.org/x/term"
	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	v1Networking "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	version = "v0.0.0"
	commit  = ""
	date    = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	//settings                   *cli.EnvSettings
	src                        rand.Source
	filterboxGithubReleasesURL = "https://api.github.com/repos/PlainsightAI/filterbox/tags"
	plainsightHelmchartURL     = "https://plainsightai.github.io/helm-charts/"
	helmInstallURL             = "https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3"
	nvidiaGPGKeyURL            = "https://nvidia.github.io/nvidia-docker/gpgkey"
	lambdaStackURL             = "https://lambdalabs.com/install-lambda-stack.sh"
	k3sInstallURL              = "https://get.k3s.io"
	repoName                   = "plainsight-technologies"
	plainsightNamespace        = "plainsight"
	nvidiaGpuOperatorNamespace = "gpu-operator"
	nvidiaHelmRepoURL          = "https://helm.ngc.nvidia.com/nvidia"
)

var installDeps = map[string]bool{
	"curl":  false,
	"wget":  false,
	"socat": false,
}

func updateAvailable() (bool, string, error) {
	latestRelease, err := latestfilterboxRelease()
	if err != nil {
		return false, "", err
	}

	currentVersion, err := semver.NewVersion(version)
	if err != nil {
		return false, "", err
	}

	availableVersion, err := semver.NewVersion(latestRelease)
	if err != nil {
		return false, "", err
	}

	if currentVersion.LessThan(availableVersion) {
		return true, availableVersion.String(), nil
	}

	return false, "", nil
}

func init() {
	src = rand.NewSource(time.Now().UnixNano())
}

func runCmd(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func getUbuntuVersion() (string, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && parts[0] == "VERSION_CODENAME" {
			return strings.Trim(parts[1], "\""), nil
		}
	}

	return "", errors.New("failed to get Ubuntu version")
}

func supportedUbuntuVersion() bool {
	supportedVersions := []string{"focal", "jammy"}
	ubuntuVersion, err := getUbuntuVersion()
	if err != nil {
		return false
	}

	for _, v := range supportedVersions {
		if ubuntuVersion == v {
			return true
		}
	}

	fmt.Printf("Invalid Ubuntu version: %s\nSupported versions: %s\n", ubuntuVersion, supportedVersions)
	return false
}

func setupNvidiaRuntimeClass() error {
	runtimeClass := &nodev1.RuntimeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "node.k8s.io/v1", Kind: "RuntimeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia"},
		Handler:    "nvidia",
	}

	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	runtimeClassClient := k8sClient.NodeV1().RuntimeClasses()
	_, err = runtimeClassClient.Create(context.Background(), runtimeClass, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	fmt.Println("RuntimeClass created successfully.")
	return nil
}

func main() {
	if !supportedUbuntuVersion() {
		fmt.Println("Ubuntu (debian/apt) Only")
		os.Exit(1)
	}

	cmds := []*urcli.Command{
		{
			Name:    "init",
			Aliases: []string{"i"},
			Usage:   "initialize filterbox device",
			Action:  initAction,
		},
		{
			Name:    "nvidia",
			Aliases: []string{"n"},
			Usage:   "initialize nvidia drivers and tools",
			Action:  nvidiaSetupAction,
		},
		{
			Name:    "registry",
			Aliases: []string{"reg"},
			Usage:   "container registry",
			Subcommands: []*urcli.Command{
				{
					Name:    "create",
					Aliases: []string{"c"},
					Usage:   "create container/docker registry secrets",
					Action:  createRegSecretAction,
				},
			},
		},
		{
			Name:    "repository",
			Aliases: []string{"repo"},
			Usage:   "helm repository",
			Subcommands: []*urcli.Command{
				{
					Name:    "create",
					Aliases: []string{"c"},
					Usage:   "create helm repository",
					Action:  createHelmRepoAction,
				},
			},
		},
		{
			Name:    "filter",
			Aliases: []string{"f"},
			Usage:   "actions for filter resources",
			Subcommands: []*urcli.Command{
				{
					Name:    "install",
					Aliases: []string{"i"},
					Usage:   "install filter on device",
					Action:  installFilterAction,
				},
				{
					Name:    "list",
					Aliases: []string{"l"},
					Usage:   "list filters installed on device",
					Action:  listFiltersAction,
				},
				{
					Name:    "stop",
					Aliases: []string{"s"},
					Usage:   "stop filter installed on device",
					Action:  stopFiltersAction,
				},
				{
					Name:    "run",
					Aliases: []string{"r"},
					Usage:   "run filter installed on device",
					Action:  startFilterAction,
				},
				{
					Name:    "uninstall",
					Aliases: []string{"u"},
					Usage:   "run filter installed on device",
					Action:  uninstallFilterAction,
				},
			},
		},
	}

	update, newVersion, err := updateAvailable()
	if err != nil {
		fmt.Printf("failed to check for update: %v\n", err.Error())
	}

	if update {
		fmt.Printf("\nNew filterbox version available: %s\n\n", newVersion)
		cmds = append(cmds, &urcli.Command{
			Name:    "update",
			Aliases: []string{"u"},
			Usage:   "update filterbox to latest version",
			Action:  updateFitlerboxAction,
		})
	}

	app := &urcli.App{
		Version:  version,
		Usage:    "Plainsight Filterbox Controller CLI",
		Commands: cmds,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func updateFilterBox() error {
	cmd := exec.Command("bash", "-c", "sudo curl -sfL https://raw.githubusercontent.com/PlainsightAI/filterbox/main/install.sh | bash -")
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func updateFitlerboxAction(c *urcli.Context) error {
	return updateFilterBox()
}

func createHelmRepoAction(c *urcli.Context) error {
	println("Input your Helm Repository Configuration")
	name := userInput("name: ")
	helmUrl := userInput("url: ")
	username := userInput("username: ")
	password := secureUserInput("password (hidden): ")

	if err := RepoAdd(name, helmUrl, username, password); err != nil {
		return err
	}
	RepoUpdate()
	return nil
}

func userInput(prompt string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print(prompt)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func secureUserInput(prompt string) string {
	fmt.Print(prompt)
	password, _ := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(password)
}

func createNvidiaTimeSlicingConfig() error {

	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "time-slicing-config-all",
			Namespace: nvidiaGpuOperatorNamespace,
		},
		Data: map[string]string{
			"any": `version: v1
flags:
  migStrategy: none
sharing:
  timeSlicing:
    resources:
    - name: nvidia.com/gpu
      replicas: 4`,
		},
	}

	// Create the ConfigMap
	result, err := k8sClient.CoreV1().ConfigMaps(nvidiaGpuOperatorNamespace).Create(context.Background(), configMap, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("Created ConfigMap %q.\n", result.GetObjectMeta().GetName())
	return nil
}

func createRegSecretAction(c *urcli.Context) error {
	println("Input your Docker Registry Server Configuration")
	server := userInput("server: ")
	username := userInput("username: ")
	password := secureUserInput("password (hidden): ")
	email := userInput("email: ")
	println("")
	println("Name your kubernetes secret for reference")
	secretName := userInput("secret-name: ")

	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	// Create a new secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: plainsightNamespace,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			".dockerconfigjson": []byte(
				fmt.Sprintf(`{"auths":{"%s":{"username":"%s","password":"%s","email":"%s","auth":"%s"}}}`,
					server,
					username,
					password,
					email,
					base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password))))),
		},
	}

	_, err = k8sClient.CoreV1().Secrets(plainsightNamespace).Create(c.Context, secret, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func uninstallFilterAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(plainsightNamespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}
	if len(deps.Items) == 0 {
		return nil
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	return UninstallChart(filterChoice, plainsightNamespace)
}

func startFilterAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(plainsightNamespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}
	if len(deps.Items) == 0 {
		return nil
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	// Fetch the deployment
	deployment, err := k8sClient.AppsV1().Deployments(plainsightNamespace).Get(ctx.Context, filterChoice, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Scale the deployment down to zero replicas
	deployment.Spec.Replicas = new(int32)
	*deployment.Spec.Replicas = 1

	_, err = k8sClient.AppsV1().Deployments(plainsightNamespace).Update(ctx.Context, deployment, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func stopFiltersAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(plainsightNamespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}
	if len(deps.Items) == 0 {
		return nil
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	// Fetch the deployment
	deployment, err := k8sClient.AppsV1().Deployments(plainsightNamespace).Get(ctx.Context, filterChoice, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Scale the deployment down to zero replicas
	deployment.Spec.Replicas = new(int32)
	*deployment.Spec.Replicas = 0

	_, err = k8sClient.AppsV1().Deployments(plainsightNamespace).Update(ctx.Context, deployment, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func listFiltersAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(plainsightNamespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}

	for _, dep := range deps.Items {
		fmt.Println(fmt.Sprintf("%s	%v", dep.Name, dep.Status.ReadyReplicas))
	}
	return nil
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

func randString(n int) string {
	b := make([]byte, n)
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return *(*string)(unsafe.Pointer(&b))
}

func installFilterAction(cCtx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	RepoUpdate()

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	filterMenu.AddItem("General Object Detection", "filter-object-detection")
	filterMenu.AddItem("Seymour Pong", "filter-pong")
	filterChoice := filterMenu.Display()

	filterVersionMenu := gocliselect.NewMenu("Choose a Filter Version")
	filterVersionMenu.AddItem("0.1.0", "0.1.0")
	filterVersionChoice := filterVersionMenu.Display()

	println("Input your Video Source?")
	println("this can be a device number for a USB Device (example: 0,1) ")
	println("this can be an RTSP Address (example: rtsp://10.100.10.20:8000/uniqueIdHere")

	rtspInput := userInput("input: ")

	args := map[string]interface{}{
		"filterName":    filterChoice,
		"filterVersion": filterVersionChoice,
		"deviceSource":  rtspInput,
	}

	name := fmt.Sprintf("%s-%s", filterChoice, strings.ToLower(randString(4)))

	return InstallChart(name, repoName, "filter", plainsightNamespace, false, args)
}

func checkHelm() error {
	_, err := exec.LookPath("helm")
	if err != nil {
		println("")
		println("installing helm (kubernetes package manager)")
		// Make an HTTP GET request to fetch the script content
		resp, err := http.Get(helmInstallURL)
		if err != nil {
			return err
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				panic(err)
			}
		}()

		// Create a temporary file to store the script
		file, err := os.CreateTemp("", "helm-script-")
		if err != nil {
			return err
		}
		defer func() {
			if err := os.Remove(file.Name()); err != nil {
				panic(err)
			}
		}()

		// Write the script content to the temporary file
		_, err = io.Copy(file, resp.Body)
		if err != nil {
			return err
		}

		// Close the file to ensure it's fully written and closed
		if err := file.Close(); err != nil {
			return err
		}

		// Make the script file executable
		err = os.Chmod(file.Name(), 0755)
		if err != nil {
			return err
		}

		// Execute the script
		cmd := exec.Command(file.Name())

		// Set the output to os.Stdout and os.Stderr to see the installation progress
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Run the command
		if cerr := cmd.Run(); cerr != nil {
			return cerr
		}
	}
	return nil
}

func lambdaStackInstall() error {
	resp, err := http.Get(lambdaStackURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			panic(err)
		}
	}()

	// Create a temporary file to store the script
	file, err := os.CreateTemp("", "lambda-stack-script-")
	if err != nil {
		return err
	}

	// Write the script content to the temporary file
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return err
	}

	// Close the file to ensure it's fully written and closed
	if err := file.Close(); err != nil {
		return err
	}

	// Make the script file executable
	err = os.Chmod(file.Name(), 0755)
	if err != nil {
		return err
	}

	_ = os.Setenv("I_AGREE_TO_THE_CUDNN_LICENSE", "1")

	// Execute the script with the specified arguments
	cmd := exec.Command("sh", file.Name())

	// Set the output to os.Stdout and os.Stderr to see the installation progress
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command
	if cerr := cmd.Run(); cerr != nil {
		return cerr
	}

	return nil
}

func checkK8s() error {
	_, err := K8sClient()
	if err != nil {
		println("")
		println("unable to connect to a kubernetes cluster")
		// Make an HTTP GET request to fetch the script content
		resp, err := http.Get(k3sInstallURL)
		if err != nil {
			return err
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				panic(err)
			}
		}()

		// Create a temporary file to store the script
		file, err := os.CreateTemp("", "k3s-script-")
		if err != nil {
			return err
		}
		defer func() {
			if err := os.Remove(file.Name()); err != nil {
				panic(err)
			}
		}()

		// Write the script content to the temporary file
		_, err = io.Copy(file, resp.Body)
		if err != nil {
			return err
		}

		// Close the file to ensure it's fully written and closed
		if err := file.Close(); err != nil {
			return err
		}

		// Make the script file executable
		err = os.Chmod(file.Name(), 0755)
		if err != nil {
			return err
		}

		// Set the environment variable for INSTALL_K3S_EXEC
		//_ = os.Setenv("INSTALL_K3S_EXEC", "--docker")

		// Execute the script with the specified arguments
		cmd := exec.Command("sh", file.Name(), "--write-kubeconfig-mode", "644")

		// Set the output to os.Stdout and os.Stderr to see the installation progress
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Run the command
		if cerr := cmd.Run(); cerr != nil {
			return cerr
		}
		_ = os.Setenv("KUBECONFIG", "/etc/rancher/k3s/k3s.yaml")
		return nil
	}

	return nil
}

func setupKubeConfig() error {

	kubeConfigPath := filepath.Join(homeDir(), ".kube", "config")

	// Set KUBECONFIG environment variable
	if err := os.Setenv("KUBECONFIG", kubeConfigPath); err != nil {
		return err
	}

	// Create ~/.kube directory (ignore error if it already exists)
	if err := os.MkdirAll(filepath.Join(homeDir(), ".kube"), 0700); err != nil {
		return err
	}

	// Run 'sudo k3s kubectl config view --raw' command
	cmd := exec.Command("sudo", "k3s", "kubectl", "config", "view", "--raw")

	// Redirect the command output to a file
	outputFile, err := os.Create(os.Getenv("KUBECONFIG"))
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := outputFile.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	cmd.Stdout = outputFile

	// Run the command
	err = cmd.Run()
	if err != nil {
		return err
	}

	// Change file permissions to 600
	err = os.Chmod(kubeConfigPath, 0600)
	if err != nil {
		return err
	}

	println(fmt.Sprintf("KUBECONFIG generated at: %s", kubeConfigPath))
	return nil
}

func initAction(cCtx *urcli.Context) error {
	if err := runCmd("sudo", "apt", "update"); err != nil {
		return err
	}

	// Check if the required dependencies are installed
	for dep, _ := range installDeps {
		if _, err := exec.LookPath(dep); err != nil {
			if err := runCmd("sudo", "apt", "install", "-y", dep); err != nil {
				return err
			}
		}
	}

	if err := lambdaStackInstall(); err != nil {
		return err
	}

	if err := checkK8s(); err != nil {
		return err
	}

	if err := checkHelm(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(cCtx.Context)
	defer cancel()

	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	if err := setupKubeConfig(); err != nil {
		return err
	}

	// Add Plainsight helm repo
	if err := RepoAdd(repoName, plainsightHelmchartURL, "", ""); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	// Update charts from the helm repo
	RepoUpdate()

	// Setup Plainsight Namaespace
	AddNamespace(ctx, plainsightNamespace, k8sClient)

	// Install NanoMQ
	nanoMQ := "nanomq"
	if err := InstallChart(nanoMQ, repoName, nanoMQ, plainsightNamespace, true, nil); err != nil {
		return err
	}

	if err := InstallIngress(ctx, nanoMQ, 8083, k8sClient); err != nil {
		return err
	}

	fmt.Println("Please run \"sudo reboot\" to complete the installation.")
	return nil
}

func InstallIngress(ctx context.Context, name string, port int32, k8sClient *kubernetes.Clientset) error {
	prefixPathTyp := v1Networking.PathTypePrefix

	_, err := k8sClient.NetworkingV1().Ingresses(plainsightNamespace).Create(ctx, &v1Networking.Ingress{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "networking.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ingress", name),
			Namespace: plainsightNamespace,
		},
		Spec: v1Networking.IngressSpec{
			Rules: []v1Networking.IngressRule{
				{
					Host: fmt.Sprintf("%s.filterbox.local", name),
					IngressRuleValue: v1Networking.IngressRuleValue{
						HTTP: &v1Networking.HTTPIngressRuleValue{
							Paths: []v1Networking.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &prefixPathTyp,
									Backend: v1Networking.IngressBackend{
										Service: &v1Networking.IngressServiceBackend{
											Name: name,
											Port: v1Networking.ServiceBackendPort{
												Number: port,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})

	return err
}

func AddNamespace(ctx context.Context, name string, client *kubernetes.Clientset) {
	// check if namespace already exists
	_, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err = client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
}

// RepoAdd adds repo with given name and url
func RepoAdd(name, url, username, password string) error {
	_ = os.Setenv("HELM_NAMESPACE", plainsightNamespace)
	settings := cli.New()

	repoFile := settings.RepositoryConfig

	//Ensure the file directory exists as it is required for file locking
	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		return err
	}

	// Acquire a file lock for process synchronization
	fileLock := flock.New(strings.Replace(repoFile, filepath.Ext(repoFile), ".lock", 1))
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, time.Second)
	if err == nil && locked {
		defer func() {
			if err := fileLock.Unlock(); err != nil {
				log.Fatal(err)
			}
		}()
	}
	if err != nil {
		log.Fatal(err)
	}

	b, err := os.ReadFile(repoFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var f repo.File
	if err := yaml.Unmarshal(b, &f); err != nil {
		log.Fatal(err)
	}

	if f.Has(name) {
		return fmt.Errorf("repository name (%s) already exists\n", name)
	}

	c := repo.Entry{Name: name, URL: url, Username: username, Password: password}

	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		return err
	}

	if _, err := r.DownloadIndexFile(); err != nil {
		return errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", url)
	}

	f.Update(&c)

	if err := f.WriteFile(repoFile, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q has been added to your repositories\n", name)
	return nil
}

// RepoUpdate updates charts for all helm repos
func RepoUpdate() {
	_ = os.Setenv("HELM_NAMESPACE", plainsightNamespace)
	settings := cli.New()
	repoFile := settings.RepositoryConfig

	f, err := repo.LoadFile(repoFile)
	if os.IsNotExist(errors.Cause(err)) || len(f.Repositories) == 0 {
		log.Fatal(errors.New("no repositories found. You must add one before updating"))
	}
	var repos []*repo.ChartRepository
	for _, cfg := range f.Repositories {
		r, err := repo.NewChartRepository(cfg, getter.All(settings))
		if err != nil {
			log.Fatal(err)
		}
		repos = append(repos, r)
	}

	fmt.Printf("Hang tight while we grab the latest from your chart repositories...\n")
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(re *repo.ChartRepository) {
			defer wg.Done()
			if _, err := re.DownloadIndexFile(); err != nil {
				fmt.Printf("...Unable to get an update from the %q chart repository (%s):\n\t%s\n", re.Config.Name, re.Config.URL, err)
			} else {
				fmt.Printf("...Successfully got an update from the %q chart repository\n", re.Config.Name)
			}
		}(re)
	}
	wg.Wait()
	fmt.Printf("Update Complete. ⎈ Happy Helming!⎈\n")
}

// UninstallChart uninstalls a Helm chart
func UninstallChart(name, namespace string) error {
	_ = os.Setenv("HELM_NAMESPACE", plainsightNamespace)
	settings := cli.New()
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), debug); err != nil {
		return err
	}

	client := action.NewUninstall(actionConfig)

	// Run uninstall
	resp, err := client.Run(name)
	if err != nil {
		return err
	}

	fmt.Printf("Chart %s uninstalled. Release status: %s\n", name, resp.Info)
	return nil
}

// InstallChart installs the helm chart
func InstallChart(name, repo, chart, namespace string, waitForDeployment bool, values map[string]interface{}) error {
	_ = os.Setenv("HELM_NAMESPACE", namespace)
	settings := cli.New()
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), debug); err != nil {
		return err
	}

	if releaseExists(name, actionConfig) {
		println(fmt.Sprintf("%s: release already exists. SKIPPING", name))
		return nil
	}

	client := action.NewInstall(actionConfig)

	if client.Version == "" && client.Devel {
		client.Version = ">0.0.0-0"
	}

	client.ReleaseName = name
	cp, err := client.ChartPathOptions.LocateChart(fmt.Sprintf("%s/%s", repo, chart), settings)
	if err != nil {
		return err
	}

	debug("CHART PATH: %s\n", cp)

	p := getter.All(settings)

	// Check chart dependencies to make sure all are present in /charts
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return err
	}

	validInstallableChart, err := isChartInstallable(chartRequested)
	if err != nil {
		return err
	}
	if !validInstallableChart {
		return fmt.Errorf("%s: chart is invalid", name)
	}

	if req := chartRequested.Metadata.Dependencies; req != nil {
		if derr := action.CheckDependencies(chartRequested, req); derr != nil {
			if client.DependencyUpdate {
				man := &downloader.Manager{
					Out:              os.Stdout,
					ChartPath:        cp,
					Keyring:          client.ChartPathOptions.Keyring,
					SkipUpdate:       false,
					Getters:          p,
					RepositoryConfig: settings.RepositoryConfig,
					RepositoryCache:  settings.RepositoryCache,
				}
				if uerr := man.Update(); uerr != nil {
					return uerr
				}
			} else {
				return derr
			}
		}
	}

	client.Timeout = 300 * time.Second
	client.Wait = waitForDeployment
	client.Namespace = settings.Namespace()
	release, err := client.Run(chartRequested, values)
	if err != nil {
		return err
	}
	fmt.Println(release.Name)
	return nil
}

// Function to check if a release with the given name already exists
func releaseExists(name string, actionConfig *action.Configuration) bool {
	client := action.NewList(actionConfig)

	// Set the namespace for listing releases
	client.AllNamespaces = true
	client.SetStateMask()

	// List releases
	releases, err := client.Run()
	if err != nil {
		log.Fatal(err)
	}

	// Check if the release with the specified name already exists
	for _, release := range releases {
		if release.Name == name {
			return true
		}
	}

	return false
}

func isChartInstallable(ch *chart.Chart) (bool, error) {
	switch ch.Metadata.Type {
	case "", "application":
		return true, nil
	}
	return false, errors.Errorf("%s charts are not installable", ch.Metadata.Type)
}

func debug(format string, v ...interface{}) {
	format = fmt.Sprintf("[debug] %s\n", format)
	err := log.Output(2, fmt.Sprintf(format, v...))
	if err != nil {
		return
	}
}

func K8sClient() (*kubernetes.Clientset, error) {

	var (
		c             *rest.Config
		err           error
		localUserPath = filepath.Join(homeDir(), ".kube", "config")
		k3sConfig     = "/etc/rancher/k3s/k3s.yaml"
	)

	// Check for valid Local user Config (~/.kube/config
	_, err = os.Stat(localUserPath)
	if err == nil {
		c, err = clientcmd.BuildConfigFromFlags("", localUserPath)
		if err != nil {
			return nil, err
		}
		client, err := kubernetes.NewForConfig(c)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get cluster config")
		}
		return client, nil
	}

	// use k3s config file instead
	_, err = os.Stat(k3sConfig)
	if err == nil {
		c, err = clientcmd.BuildConfigFromFlags("", k3sConfig)
		if err != nil {
			return nil, err
		}
		client, err := kubernetes.NewForConfig(c)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get cluster config")
		}
		return client, nil
	}

	return nil, errors.New("failed to get kubernetes client")
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}

	return os.Getenv("USERPROFILE") // windows
}

// Tag represents the structure of the JSON response
type Tag struct {
	Name string `json:"name"`
	// Add other fields if needed
}

func latestfilterboxRelease() (string, error) {
	// URL of the GitHub API endpoint

	// Make the HTTP GET request
	response, err := http.Get(filterboxGithubReleasesURL)
	if err != nil {
		return version, err
	}
	defer response.Body.Close()

	// Read the response body
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return version, err
	}

	// Parse the JSON response into a slice of Tag structs
	var tags []Tag
	err = json.Unmarshal(body, &tags)
	if err != nil {
		return version, err
	}

	// Check if there's at least one tag
	if len(tags) > 0 {
		// Extract the name of the first tag
		return tags[0].Name, nil
	}
	return version, nil
}

func getDistribution() (string, error) {
	cmd := exec.Command("bash", "-c", ". /etc/os-release; echo $ID$VERSION_ID")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func addNvidiaKey() error {
	resp, err := http.Get(nvidiaGPGKeyURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			panic(err)
		}
	}()

	key, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	cmd := exec.Command("sudo", "apt-key", "add", "-")
	cmd.Stdin = strings.NewReader(string(key))
	err = cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func addNvidiaRepo(distribution string) error {
	repoURL := fmt.Sprintf("https://nvidia.github.io/nvidia-docker/%s/nvidia-docker.list", distribution)
	resp, err := http.Get(repoURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			panic(err)
		}
	}()

	r, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	cmd := exec.Command("sudo", "tee", "/etc/apt/sources.list.d/nvidia-docker.list")
	cmd.Stdin = strings.NewReader(string(r))
	err = cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func nvidiaSetupAction(c *urcli.Context) error {
	//cmd := exec.Command("nvidia-smi")
	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr
	//err := cmd.Run()
	//if err != nil {
	//	fmt.Println("No Nvidia GPU in system!")
	//	return err
	//}

	distribution, err := getDistribution()
	if err != nil {
		return err
	}

	err = addNvidiaKey()
	if err != nil {
		return err
	}

	err = addNvidiaRepo(distribution)
	if err != nil {
		return err
	}

	fmt.Println("NVIDIA repository added successfully.")

	if err := runCmd("sudo", "apt", "update"); err != nil {
		return err
	}

	// Check if the required dependencies are installed
	if err := runCmd("sudo", "apt", "install", "-y", "nvidia-container-runtime"); err != nil {
		return err
	}

	fmt.Println("NVIDIA nvidia-container-runtime installed successfully.")

	if err := setupK3sNvidiaContainerRuntime(); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	fmt.Println("NVIDIA nvidia-container-runtime setup successfully.")

	if err := setupNvidiaRuntimeClass(); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	// Add NVIDIA Helm Repo
	if err := RepoAdd("nvidia", nvidiaHelmRepoURL, "", ""); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	// Update charts from the helm repo
	RepoUpdate()

	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	// Setup Nvidia Namaespace
	AddNamespace(c.Context, nvidiaGpuOperatorNamespace, k8sClient)

	values := map[string]interface{}{
		"toolkit": map[string]interface{}{
			"env": []map[string]interface{}{
				{
					"name":  "CONTAINERD_CONFIG",
					"value": "/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
				},
				{
					"name":  "CONTAINERD_SOCKET",
					"value": "/run/k3s/containerd/containerd.sock",
				},
				{
					"name":  "CONTAINERD_RUNTIME_CLASS",
					"value": "nvidia",
				},
				{
					"name":  "CONTAINERD_SET_AS_DEFAULT",
					"value": "true",
				},
			},
		},
	}

	// Install Nvidia GPU Operator
	if err := InstallChart("gpu-operator", "nvidia", "gpu-operator", nvidiaGpuOperatorNamespace, true, values); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	if err := createNvidiaTimeSlicingConfig(); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	if err := patchNvidiaGPUClusterPolicy(c.Context); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	return nil
}

func patchNvidiaGPUClusterPolicy(ctx context.Context) error {
	// Define the kubectl command and arguments
	command := "kubectl"
	args := []string{"-n", "gpu-operator", "patch", "clusterpolicy/cluster-policy", "--type", "merge", "-p", `{"spec": {"devicePlugin": {"config": {"name": "time-slicing-config-all", "default": "any"}}}}`}

	// Execute the command
	cmd := exec.Command(command, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}

	println(string(output))
	return nil
}

func setupK3sNvidiaContainerRuntime() error {
	// Define the content of the file
	const configContent = `{{ template "base" . }}

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes."custom"]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes."custom".options]
  BinaryName = "/usr/bin/nvidia-container-runtime"`

	// Use sudo to create or open the file for writing
	cmd := exec.Command("sudo", "tee", "/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl")
	cmd.Stdin = strings.NewReader(configContent)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
