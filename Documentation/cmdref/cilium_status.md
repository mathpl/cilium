<!-- This file was autogenerated via cilium cmdref, do not edit manually-->

## cilium status

Display status

```
cilium status [flags]
```

### Options

```
  -h, --help                     help for status
      --ignore-warnings          Ignore warnings when waiting for status to report success
      --interactive              Refresh the status summary output after each retry when --wait flag is specified (default true)
  -o, --output string            Output format. One of: json, summary (default "summary")
      --verbose                  Print more verbose error / log messages
      --wait                     Wait for status to report success (no errors and warnings)
      --wait-duration duration   Maximum time to wait for status (default 5m0s)
      --worker-count int         The number of workers to use (default 5)
```

### Options inherited from parent commands

```
      --as string                  Username to impersonate for the operation. User could be a regular user or a service account in a namespace.
      --as-group stringArray       Group to impersonate for the operation, this flag can be repeated to specify multiple groups.
      --context string             Kubernetes configuration context
      --helm-release-name string   Helm release name (default "cilium")
      --kubeconfig string          Path to the kubeconfig file
  -n, --namespace string           Namespace Cilium is running in (default "kube-system")
```

### SEE ALSO

* [cilium](cilium.md)	 - Cilium provides eBPF-based Networking, Security, and Observability for Kubernetes

