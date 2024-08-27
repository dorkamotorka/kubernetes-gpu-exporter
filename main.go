package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
	"strings"
	"os/exec"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	podGpuMemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pod_gpu_memory_usage",
			Help: "GPU memory used by Kubernetes Pod",
		},
		[]string{"pid", "pod"},
	)
	podGpuMemoryPercUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "docker_gpu_memory_perc_usage",
			Help: "GPU memory in percentage used by pod",
		},
		[]string{"pid", "pod"},
	)
)

func main() {
	// Register Prometheus metrics
	reg := prometheus.NewRegistry()
	reg.MustRegister(podGpuMemoryUsed)
	reg.MustRegister(podGpuMemoryPercUsed)

	// Initialize NVML
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		log.Fatalf("Unable to initialize NVML: %v", nvml.ErrorString(ret))
	}
	defer func() {
		ret := nvml.Shutdown()
		if ret != nvml.SUCCESS {
			log.Fatalf("Unable to shutdown NVML: %v", nvml.ErrorString(ret))
		}
	}()

	// Create a Kubernete client
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Start Prometheus metrics server
	go func() {
		handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
		http.Handle("/metrics", handler)
		log.Fatal(http.ListenAndServe(":8000", nil))
	}()

	for {
		// List running containers
		// get pods in all the namespaces by omitting namespace
		pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("There are %d pods in the cluster\n", len(pods.Items))

		// Create a map to store PIDs with pod names as keys
    		podPIDMap := make(map[string][]string)
		for _, pod := range pods.Items {
			namespace := pod.Namespace
			podName := pod.Name

			fmt.Printf("Pod: %s/%s\n", namespace, podName)

			var pids []string
			for _, container := range pod.Status.ContainerStatuses {
			    	containerID := container.ContainerID

			    	// Extract the container ID (trim off the "docker://" or similar prefix)
			    	if len(containerID) > 0 {
					containerID = containerID[strings.Index(containerID, "://")+3:]
			   	}

			    	// Use "kubectl exec" to run "ps" command inside the container to list PIDs
			    	cmd := exec.Command("kubectl", "exec", "-n", namespace, podName, "--", "ps", "-e", "-o", "pid=")
			    	output, err := cmd.CombinedOutput()
			    	if err != nil {
					log.Printf("Failed to get PIDs for container %s in pod %s/%s: %v", containerID, namespace, podName, err)
					continue
			    	}

			    	fmt.Printf("PIDs in container %s:\n%s\n", containerID, output)
				pids = append(pids, strings.Fields(string(output))...)
			}

			// Store the PIDs in the map with the pod name as the key
        		podPIDMap[podName] = pids
		}

		// Get device count
		count, ret := nvml.DeviceGetCount()
		if ret != nvml.SUCCESS {
			log.Fatalf("Unable to get device count: %v", nvml.ErrorString(ret))
		}

		// Iterate over devices
		for di := 0; di < count; di++ {
			device, ret := nvml.DeviceGetHandleByIndex(di)
			if ret != nvml.SUCCESS {
				log.Fatalf("Unable to get device at index %d: %v", di, nvml.ErrorString(ret))
			}

			memoryInfo, ret := device.GetMemoryInfo()
			if ret != nvml.SUCCESS {
				log.Fatalf("Unable to get device memory at index %d: %v", di, nvml.ErrorString(ret))
			}

			// Get running processes on device
			processInfos, ret := device.GetComputeRunningProcesses()
			if ret != nvml.SUCCESS {
				log.Fatalf("Unable to get process info for device at index %d: %v", di, nvml.ErrorString(ret))
			}

			// Iterate over running processes
			for _, processInfo := range processInfos {
				// Iterate over pod PIDs
				for podName, pids := range podPIDMap {
					for pid := range pids {
						if pid == int(processInfo.Pid) {
							// Set Prometheus metrics
							podGpuMemoryUsed.WithLabelValues(fmt.Sprintf("%d", pid), podName).Set(float64(processInfo.UsedGpuMemory))

							percent := (float64(processInfo.UsedGpuMemory) / float64(memoryInfo.Total)) * 100
							podGpuMemoryPercUsed.WithLabelValues(fmt.Sprintf("%d", pid), podName).Set(percent)
						}
					}
				}
			}
		}
		time.Sleep(30 * time.Second)
	}
}
