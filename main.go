package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"github.com/nexidian/gocliselect"
	"github.com/pkg/errors"
	urcli "github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	v1Networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	version   = "unknown"
	commit    = "unknown"
	date      = "unknown"
	settings  *cli.EnvSettings
	src       rand.Source
	url       = "https://plainsightai.github.io/helm-charts/"
	repoName  = "plainsight-technologies"
	namespace = "plainsight"
)

func init() {
	_ = os.Setenv("HELM_NAMESPACE", namespace)
	settings = cli.New()
	src = rand.NewSource(time.Now().UnixNano())
}

func main() {

	// Retrieve information about the host OS and architecture
	operatingSys := runtime.GOOS
	arch := runtime.GOARCH

	if operatingSys != "linux" {
		println("Currently only linux is supported")
		os.Exit(1)
	}

	var validArch = false

	switch arch {
	case "amd64", "x86_64", "arm64":
		validArch = true
	}

	if !validArch {
		println(fmt.Sprintf("Detected Linux but unsupported architecture: %s", arch))
		os.Exit(1)
	}

	app := &urcli.App{
		Version: version,
		Usage:   "Plainsight Filterbox Controller CLI",
		Commands: []*urcli.Command{
			{
				Name:    "init",
				Aliases: []string{"i"},
				Usage:   "initialize filterbox device",
				Action:  initAction,
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
		},
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func uninstallFilterAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(namespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	return UninstallChart(filterChoice)
}

func startFilterAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(namespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	// Fetch the deployment
	deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx.Context, filterChoice, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Scale the deployment down to zero replicas
	deployment.Spec.Replicas = new(int32)
	*deployment.Spec.Replicas = 1

	_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx.Context, deployment, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func dockerSetup() error {
	// Check if Docker is already installed
	if _, err := exec.LookPath("docker"); err == nil {
		return nil
	}

	// Attempt to install Docker
	fmt.Println("Docker is not installed. Attempting to install...")

	// Update APT
	updateAPTCmd := exec.Command("sudo", "apt", "update")
	updateAPTCmd.Stdout = os.Stdout
	updateAPTCmd.Stderr = os.Stderr
	if err := updateAPTCmd.Run(); err != nil {
		return errors.New("failed to update APT")
	}

	// Run installation commands
	installCmd := exec.Command("sudo", "apt", "install", "docker.io", "-y")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return errors.New("failed to install Docker, please install it manually")
	}

	// Add the user to the docker group
	usermodCmd := exec.Command("sudo", "usermod", "-aG", "docker", os.Getenv("USER"))
	usermodCmd.Stdout = os.Stdout
	usermodCmd.Stderr = os.Stderr
	if err := usermodCmd.Run(); err != nil {
		return errors.New("failed to add user to the docker group, please add manually")
	}

	fmt.Println("Docker has been installed successfully.")
	return nil
}

func stopFiltersAction(ctx *urcli.Context) error {
	k8sClient, err := K8sClient()
	if err != nil || k8sClient == nil {
		return err
	}

	deps, err := k8sClient.AppsV1().Deployments(namespace).List(ctx.Context, metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{"app.kubernetes.io/name": "filter"}).String(),
	})
	if err != nil {
		return err
	}

	filterMenu := gocliselect.NewMenu("Choose a Filter")
	for _, dep := range deps.Items {
		filterMenu.AddItem(dep.Name, dep.Name)
	}
	filterChoice := filterMenu.Display()

	// Fetch the deployment
	deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx.Context, filterChoice, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Scale the deployment down to zero replicas
	deployment.Spec.Replicas = new(int32)
	*deployment.Spec.Replicas = 0

	_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx.Context, deployment, metav1.UpdateOptions{})
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

	deps, err := k8sClient.AppsV1().Deployments(namespace).List(ctx.Context, metav1.ListOptions{
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
	filterMenu.AddItem("Face Blur", "filter-blur")
	filterMenu.AddItem("General Object Detection", "filter-object-detection")
	filterMenu.AddItem("Seymour Pong", "filter-pong")
	filterChoice := filterMenu.Display()

	filterVersionMenu := gocliselect.NewMenu("Choose a Filter Version")
	filterVersionMenu.AddItem("0.1.0", "0.1.0")
	filterVersionChoice := filterVersionMenu.Display()

	println("Input your Video Source?")
	println("this can be a device number for a USB Device (example: 0,1) ")
	println("or this can be an RTSP Address (example: rtsp://10.100.10.20:8000/uniqueIdHere")
	reader := bufio.NewReader(os.Stdin)
	// ReadString will block until the delimiter is entered
	rtspInput, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	// remove the delimeter from the string
	rtspInput = strings.TrimSuffix(rtspInput, "\n")

	args := map[string]interface{}{
		"filterName":    filterChoice,
		"filterVersion": filterVersionChoice,
		"deviceSource":  rtspInput,
	}

	name := fmt.Sprintf("%s-%s", filterChoice, strings.ToLower(randString(4)))

	return InstallChart(name, repoName, "filter", args)
}

func checkHelm() error {
	_, err := exec.LookPath("helm")
	if err != nil {
		println("")
		println("unable to find helm (kubernetes package manager)")
		println("would you like to install helm? (Y/n)")
		reader := bufio.NewReader(os.Stdin)
		// ReadString will block until the delimiter is entered
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		// remove the delimeter from the string
		input = strings.TrimSuffix(input, "\n")

		if input == "" || strings.ToLower(input) == "y" || strings.ToLower(input) == "yes" {
			cmd := exec.Command("sh", "-c", "curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash")

			// Set the output to os.Stdout and os.Stderr to see the installation progress
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			// Run the command
			if cerr := cmd.Run(); cerr != nil {
				return cerr
			}
			return nil
		}
		println("helm is required")
		return errors.New("please install helm and try again")
	}
	return nil
}

func checkK8s() error {
	_, err := K8sClient()
	if err != nil {
		println("")
		println("unable to connect to kubernetes cluster")
		println("would you like to install k3s? (Y/n)")
		reader := bufio.NewReader(os.Stdin)
		// ReadString will block until the delimiter is entered
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		// remove the delimeter from the string
		input = strings.TrimSuffix(input, "\n")

		if input == "" || strings.ToLower(input) == "y" || strings.ToLower(input) == "yes" {
			cmd := exec.Command("sh", "-c", "curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC=\"--docker\" sh -s - --write-kubeconfig-mode 644")

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
		println("kubernetes is required")
		return errors.New("please install kubernetes and try again")
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

	if err := dockerSetup(); err != nil {
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
		println("failed to find k8s client config")
		println("make sure ~/.kube/config is setup properly")
		return err
	}

	if err := setupKubeConfig(); err != nil {
		return err
	}

	// Add helm repo
	RepoAdd(repoName, url)
	// Update charts from the helm repo
	RepoUpdate()
	// Setup Plainsight Namaespace
	AddNamespace(ctx, k8sClient)
	// Install NanoMQ
	nanoMQ := "nanomq"
	if err := InstallChart(nanoMQ, repoName, nanoMQ, nil); err != nil {
		return err
	}
	if err := InstallIngress(ctx, nanoMQ, 8083, k8sClient); err != nil {
		return err
	}
	return nil
}

func InstallIngress(ctx context.Context, name string, port int32, k8sClient *kubernetes.Clientset) error {
	prefixPathTyp := v1Networking.PathTypePrefix

	_, err := k8sClient.NetworkingV1().Ingresses(namespace).Create(ctx, &v1Networking.Ingress{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "networking.k8s.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ingress", name),
			Namespace: namespace,
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

func AddNamespace(ctx context.Context, client *kubernetes.Clientset) {
	// check if namespace already exists
	_, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		return
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err = client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
}

// RepoAdd adds repo with given name and url
func RepoAdd(name, url string) {
	repoFile := settings.RepositoryConfig

	//Ensure the file directory exists as it is required for file locking
	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		log.Fatal(err)
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
		log.Fatal(err)
	}

	var f repo.File
	if err := yaml.Unmarshal(b, &f); err != nil {
		log.Fatal(err)
	}

	if f.Has(name) {
		fmt.Printf("repository name (%s) already exists\n", name)
		return
	}

	c := repo.Entry{Name: name, URL: url}

	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		log.Fatal(err)
	}

	if _, err := r.DownloadIndexFile(); err != nil {
		err := errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", url)
		log.Fatal(err)
	}

	f.Update(&c)

	if err := f.WriteFile(repoFile, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q has been added to your repositories\n", name)
}

// RepoUpdate updates charts for all helm repos
func RepoUpdate() {
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
func UninstallChart(name string) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), debug); err != nil {
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
func InstallChart(name, repo, chart string, values map[string]interface{}) error {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), debug); err != nil {
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

	client.Namespace = settings.Namespace()
	release, err := client.Run(chartRequested, values)
	if err != nil {
		return err
	}
	fmt.Println(release.Manifest)
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

func latestfilterboxRelease() {
	// URL of the GitHub API endpoint
	url := "https://api.github.com/repos/PlainsightAI/filterbox/tags"

	// Make the HTTP GET request
	response, err := http.Get(url)
	if err != nil {
		fmt.Println("Error making HTTP request:", err)
		return
	}
	defer response.Body.Close()

	// Read the response body
	body, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Println("Error reading response body:", err)
		return
	}

	// Parse the JSON response into a slice of Tag structs
	var tags []Tag
	err = json.Unmarshal(body, &tags)
	if err != nil {
		fmt.Println("Error parsing JSON:", err)
		return
	}

	// Check if there's at least one tag
	if len(tags) > 0 {
		// Extract the name of the first tag
		firstTagName := tags[0].Name

		// Print the result
		fmt.Println(firstTagName)
	} else {
		fmt.Println("No tags found.")
	}
}

func checkfilterboxVersion() {
	if version != "unknown" {

	}
}
