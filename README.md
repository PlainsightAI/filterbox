## filterbox Overview and Quickstart

This guide provides technical users with instructions on setting up and using filters within an edge computing image processing system. The system architecture includes K3s, Helm, and NanoMQ. The process is illustrated using the example of a Face Blur filter.

## Prerequisites

Familiarity with Kubernetes, Helm, and command-line interfaces.

Familiarity with MQTT

## System Components

K3s: A lightweight Kubernetes distribution for managing containerized applications.

Helm: A package manager for Kubernetes, used for deploying applications.

NanoMQ: A lightweight MQTT message broker for efficient message handling.

## Installation and Configuration

Step 1: Initialize the System
Install filterbox:

### Download the tool:



ARCH=arm64 

VERSION=0.9.0
``` bash
wget "https://github.com/PlainsightAI/filterbox/releases/download/v$VERSION/filterbox_Linux_$ARCH.tar.gz"
```

ARCH=x86_64

VERSION=0.9.0
``` bash 
wget "https://github.com/PlainsightAI/filterbox/releases/download/v$VERSION/filterbox_Linux_$ARCH.tar.gz"
```

**Extract the tarball**:


```bash
tar -xf filterbox_Linux_$ARCH.tar.gz
```

**Initialize filterbox:**


```bash
./filterbox init
 ```

## Step 2: Installing and Managing Filters

### Install the filter:


```bash
./filterbox filter install
```

## Step 3: Manage Filters using filterbox

### Run a Filter:

```bash
./filterbox filter run
```

### Stop a Filter:

```bash
./filterbox filter stop
```

### List Filters:

```bash
./filterbox filter list
```

### Uninstall a Filter:

```bash
./filterbox filter uninstall
```

## Best Practices

Ensure all prerequisites are met before beginning installation.

Regularly update Helm and Kubernetes to their latest versions.

Monitor system performance and adjust configurations as necessary.

## Recommended Resources
K3s Official Documentation

Comprehensive guide on installation, configuration, and cluster management.

Helm Official Documentation

Detailed instructions on chart development, repository management, and usage.

NanoMQ Official Documentation

NanoMQ : Source code, setup instructions, and community discussions.

## Quick Start Guide

After installing filterbox, use this Cheatsheet to interact with FilterBox, Filters, and kubernetes environment ***(commands and process may vary on some edge devices, your milage may vary)***

### Commands & Tips

**Once FilterBox is installed via filterbox, use filterbox to install a filter**

```bash
./filterbox filter install
```

**Follow in-tool prompts to choose a filter and complete any necessary config choices such as choosing a filter and version as well as assigning a video stream url or selecting a sensor**

> **Important Tip:** Using filter requires either entering a video feed url in filterbox or to select a video device on the system where you are running Filterbox
		
### Confirm the appropriate resources

Get all pods across all namespaces to identify if the filter pod is running appropriately

```bash
kubectl get pods --all-namespaces
```

**Identify the appropriate pod and note the pod name, it will be used in the next step.**


Once you confirm the appropriate pod is running, you can tail the logs to ensure that the filter is running appropriately and not spewing errors.

> **Important Tip:** add the tail extension to your kubectl config with Krew 

```bash
kubectl tail -p filter-blur-jzeq-56bc8f4756-2rp8s
```

> **Important Tip: (your pod name will be unique, get that info from your terminal)**

Check for all services running to identify the appropriate service to forward

```bash
kubectl get services --all-namespaces
```

> **Important Tip:** Note the svc name, it will be used in the next stage. 

### Port-Forward to Localhost for Confirmation 

Use Port-forwarding from the service for the target filter to localhost

>**Important Tip:** Your service name will be unique 

```bash
kubectl -n plainsight port-forward svc/filter-blur-jzeq --address 0.0.0.0 8080:8080
```

**Navigate in your browser to localhost and the port that you specified.
navigate to ```video_feed/<your device id> ``` to view the video feed from your sensor + filter** 
```
http://localhost:8080/video_feed/0
```
