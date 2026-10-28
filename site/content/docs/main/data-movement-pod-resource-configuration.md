---
title: "Data Movement Pod Resource Configuration"
layout: docs
---

During [CSI Snapshot Data Movement][1], Velero built-in data mover launches data mover pods to run the data transfer.  
During [fs-backup][2], Velero also launches data mover pods to run the data transfer.  
The data transfer is a time and resource consuming activity.  

Velero by default uses the [BestEffort QoS][2] for the data mover pods, which guarantees the best performance of the data movement activities. On the other hand, it may take lots of cluster resource, i.e., CPU, memory, ephemeral storage, and how many resources are taken is decided by the concurrency and the scale of data to be moved.  

If the cluster nodes don't have sufficient resource, Velero also allows you to customize the resources for the data mover pods.    
Note: If less resources are assigned to data mover pods, the data movement activities may take longer time; or the data mover pods may be OOM killed if the assigned memory resource doesn't meet the requirements. Consequently, the dataUpload/dataDownload may run longer or fail.  

Refer to [Performance Guidance][3] for a guidance of performance vs. resource usage, and it is highly recommended that you perform your own testing to find the best resource limits for your data.  

Velero introduces a new section in the node-agent configMap, called ```podResources```, through which you can set customized resources configurations for data mover pods.  
If it is not there, a configMap should be created manually. The configMap should be in the same namespace where Velero is installed. If multiple Velero instances are installed in different namespaces, there should be one configMap in each namespace which applies to node-agent in that namespace only. The name of the configMap should be specified in the node-agent server parameter ```--node-agent-configmap```.  
Node-agent server checks these configurations at startup time. Therefore, you could edit this configMap any time, but in order to make the changes effective, node-agent server needs to be restarted.  

### Pod Resources
Here is a sample of the configMap with ```podResources```:  
```json
{
    "podResources": {
        "cpuRequest": "1000m",
        "cpuLimit": "1000m",
        "ephemeralStorageRequest": "5Gi",
        "ephemeralStorageLimit": "10Gi",
        "memoryRequest": "512Mi",
        "memoryLimit": "1Gi"        
    }
}
```

The string values in ```podResources``` must match Kubernetes Quantity expressions; for each resource, the "request" value must not be larger than the "limit" value. Otherwise, if any one of the values fail, the entire ```podResources``` configuration will be ignored (so the default policy will be used).  

To create the configMap, save something like the above sample to a json file and then run below command:
```
kubectl create cm node-agent-config -n velero --from-file=<json file name>
```

To provide the configMap to node-agent, edit the node-agent daemonset and add the ```- --node-agent-configmap``` argument to the spec:
1. Open the node-agent daemonset spec  
```
kubectl edit ds node-agent -n velero
```
2. Add ```- --node-agent-configmap``` to ```spec.template.spec.containers```  
```
spec:
  template:
    spec:
      containers:
      - args:
        - --node-agent-configmap=<configMap name>
```

### Memory Usage and Upload Concurrency

CPU, memory and upload concurrency are separate settings. `podResources` sets the CPU and memory for data mover pods. [`--parallel-files-upload`][5] and [`--parallel-blocks-upload`][6] set how many uploads run concurrently within each volume backup, for the file system data mover and the block data mover respectively. Set them per backup or schedule.

Uploads are asynchronous: data remains in memory until it has been written to the backup repository. So memory usage of a data mover pod grows with upload concurrency. If the concurrency is not set, it defaults to `GOMAXPROCS` of the data mover process, which the Go runtime derives from the CPU limit of the pod if one is set (see [Container-aware GOMAXPROCS][7]), or otherwise from the number of CPUs on the node. Since data mover pods have no CPU limit by default, nodes with many CPUs get a high default concurrency and high memory usage.

Velero does not cap the concurrency on purpose: the best value depends on where the bottleneck of the data path is, which differs between environments. Throughput is limited by the slowest of: reading the source volume, compressing and hashing the data (CPU), and writing to the backup storage (network bandwidth and the storage itself).

Raising the concurrency increases the throughput when the data mover pod has spare CPU, memory and network bandwidth, and each upload spends time waiting rather than working, for example:
- backup storage with high bandwidth but high latency per request, such as remote object storage;
- a fast network to the backup storage that a few concurrent uploads can't fill.

Lowering the concurrency reduces memory and CPU usage without reducing the throughput when the bottleneck is elsewhere, for example:
- the network bandwidth to the backup storage, or the backup storage itself, is already saturated;
- reading the source volume is the slowest step;
- the concurrency is already higher than the number of CPUs the pod can use, since compressing and hashing are CPU bound.

In those cases extra concurrency only keeps more data in memory. With limited memory, especially on nodes with many CPUs, lower the concurrency to avoid the data mover pod being OOM killed.

If you set `memoryLimit`, also set the upload concurrency for the backup or schedule instead of relying on the default. Changing `cpuLimit` also changes the default concurrency, but it is not the intended way to control memory usage.

For the block data mover, memory usage also grows with the provisioned size of the volume, so large volumes need more memory at any concurrency.

The following results are from one environment and are only an example: incremental backup with the block data mover of a 100Gi Ceph RBD volume with 4 GiB of new data, on a 96-CPU node, with `memoryLimit: 1Gi`. The concurrency was the default (`GOMAXPROCS`), changed by setting a CPU limit on the data mover pod.

| Concurrency | Peak memory | Result | Duration |
|---|---|---|---|
| 96 (no CPU limit) | over 900 MiB | OOM killed | - |
| 16 | 928 MiB | Completed | 43 s |
| 8 | 690 MiB | Completed | 43 s |
| 2 | 384 MiB | Completed | 44 s |

In this environment, throughput did not improve above a concurrency of 2, probably because it was limited by the network bandwidth to the backup storage (an in-cluster object store), not by CPU or concurrency. With more network bandwidth, the same CPU setup and a higher concurrency can reach a higher throughput, at the cost of more memory. Run your own tests to find the right values for your network, data and backup storage.

For restores, [`--parallel-files-download`][8] sets the concurrency of the file system data mover in the same way. The block data mover has no restore concurrency setting.

### Priority Class

Data mover pods will use the priorityClassName configured in the node-agent configmap. The priorityClassName for data mover pods is configured through the node-agent configmap (specified via the `--node-agent-configmap` flag), while the node-agent daemonset itself uses the priority class set by the `--node-agent-priority-class-name` flag during Velero installation.

#### When to Use Priority Classes

**Higher Priority Classes** (e.g., `system-cluster-critical`, `system-node-critical`, or custom high-priority):
- When you have dedicated nodes for backup operations
- When backup/restore operations are time-critical
- When you want to ensure data mover pods are scheduled even during high cluster utilization
- For disaster recovery scenarios where restore speed is critical

**Lower Priority Classes** (e.g., `low-priority` or negative values):
- When you want to protect production workload performance
- When backup operations can be delayed during peak hours
- When cluster resources are limited and production workloads take precedence
- For non-critical backup operations that can tolerate delays

#### Consequences of Priority Class Settings

**High Priority**:
- ✅ Data mover pods are more likely to be scheduled quickly
- ✅ Less likely to be preempted by other workloads
- ❌ May cause resource pressure on production workloads
- ❌ Could lead to production pod evictions in extreme cases

**Low Priority**:
- ✅ Production workloads are protected from resource competition
- ✅ Cluster stability is maintained during backup operations
- ❌ Backup/restore operations may take longer to start
- ❌ Data mover pods may be preempted, causing backup failures
- ❌ In resource-constrained clusters, backups might not run at all

#### Example Configuration

To configure priority class for data mover pods, include it in your node-agent configmap:

```json
{
    "priorityClassName": "backup-priority"
}
```

First, create the priority class in your cluster:

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: backup-priority
value: 1000
globalDefault: false
description: "Priority class for Velero data mover pods"
```

Then create or update the node-agent configmap:

```bash
kubectl create cm node-agent-config -n velero --from-file=node-agent-config.json
```

**Note**: If the specified priority class doesn't exist in the cluster when data mover pods are created, the pods will fail to schedule. Velero validates the priority class at startup and logs a warning if it doesn't exist, but the pods will still attempt to use it.

### Pod Labels
Add customized labels for data mover pods to support third-party integrations and environment-specific requirements.

If `podLabels` is configured, it supersedes Velero's [in-tree third-party labels](https://github.com/velero-io/velero/blob/94f64639cee09c5caaa65b65ab5f42175f41c101/pkg/util/third_party.go#L19-L21).
If `podLabels` is not configured, Velero uses the in-tree third-party labels for compatibility with common cloud providers and networking solutions.

The configurations work for DataUpload, DataDownload, PodVolumeBackup, and PodVolumeRestore pods.

#### Configuration Example
```json
{
  "podLabels": {
    "spectrocloud.com/connection": "proxy",
    "gnp/k8s-api-access": "",
    "gnp/monitoring-client": "",
    "np/s3-backup-backend": "",
    "cp/inject-truststore": "extended"
  }
}
```

### Pod Annotations
Add customized annotations for data mover pods to support third-party integrations and pod-level configuration.

If `podAnnotations` is configured, it supersedes Velero's [in-tree third-party annotations](https://github.com/velero-io/velero/blob/94f64639cee09c5caaa65b65ab5f42175f41c101/pkg/util/third_party.go#L23-L25).
If `podAnnotations` is not configured, Velero uses the in-tree third-party annotations for compatibility with common cloud providers and networking solutions.

The configurations work for DataUpload, DataDownload, PodVolumeBackup, and PodVolumeRestore pods.

#### Configuration Example
```json
{
  "podAnnotations": {
    "iam.amazonaws.com/role": "velero-backup-role",
    "vault.hashicorp.com/agent-inject": "true",
    "prometheus.io/scrape": "true",
    "custom.company.com/environment": "production"
  }
}
```

## Related Documentation

- [Node-agent Configuration](supported-configmaps/node-agent-configmap.md) - Complete reference for all configuration options
- [Node-agent Concurrency](node-agent-concurrency.md) - Configure concurrent operations per node
- [Node Selection for Data Movement](data-movement-node-selection.md) - Configure which nodes run data movement
- [Data Movement Pod Resource Configuration](data-movement-pod-resource-configuration.md) - Configure pod resources
- [BackupPVC Configuration](data-movement-backup-pvc-configuration.md) - Configure backup storage
- [RestorePVC Configuration](data-movement-restore-pvc-configuration.md) - Configure restore storage
- [Cache PVC Configuration](data-movement-cache-volume.md) - Configure restore data mover storage

[1]: csi-snapshot-data-movement.md
[2]: file-system-backup.md
[3]: https://kubernetes.io/docs/concepts/workloads/pods/pod-qos/
[4]: performance-guidance.md
[5]: backup-reference.md#parallel-files-upload
[6]: backup-reference.md#parallel-blocks-upload
[7]: https://tip.golang.org/doc/go1.25#container-aware-gomaxprocs:~:text=Runtime%C2%B6-,Container%2Daware%20GOMAXPROCS,-%C2%B6
[8]: restore-reference.md#parallel-files-download
