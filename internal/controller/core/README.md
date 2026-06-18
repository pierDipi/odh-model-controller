# Core Controllers

## ConfigMap Controller (`configmap_controller.go`)

Watches three ConfigMaps and consolidates their CA certificate data into a single
ConfigMap consumed by KServe for secure model downloads.

### Watched ConfigMaps

| ConfigMap                     | Purpose                                       |
|-------------------------------|-----------------------------------------------|
| `odh-trusted-ca-bundle`       | Cluster-wide trusted CA certificates from ODH |
| `odh-kserve-custom-ca-bundle` | The output ConfigMap (also watched for drift) |
| Service CA ConfigMap          | OpenShift service-serving CA                  |

### Reconciliation Logic

1. Iterates over all CA bundle source ConfigMaps (sorted by name for determinism).
2. Extracts and concatenates cert PEM data into a single string.
3. Writes the result to `odh-kserve-custom-ca-bundle` with key `cabundle.crt`.
4. On any change (create/update/delete), deletes the `storage-config` Secret so it
   gets regenerated with the updated certs.

### End-to-End Flow into KServe

```
odh-trusted-ca-bundle ──┐
                         ├─ ConfigMapReconciler ──→ odh-kserve-custom-ca-bundle
service-ca-bundle ───────┘        (this controller)    (controller namespace)
                                                              │
                                                              ▼
                                           KServe CaBundleConfigMapReconciler
                                           (reads caBundleConfigMapName from
                                            inferenceservice-config)
                                                              │
                                                              ▼
                                               global-ca-bundle ConfigMap
                                                  (per user namespace)
                                                              │
                                                              ▼
                                           StorageInitializerInjector webhook
                                           mounts volume + sets env vars
                                                              │
                                                              ▼
                                         /etc/ssl/custom-certs/cabundle.crt
                                         (inside storage-initializer init container)
```

**KServe side (key files):**

- `pkg/controller/v1beta1/inferenceservice/reconcilers/cabundleconfigmap/` — copies the
  ConfigMap from the controller namespace into each user namespace as `global-ca-bundle`.
- `pkg/webhook/admission/pod/storage_initializer_injector.go` — mounts the ConfigMap as
  a volume and sets `CA_BUNDLE_CONFIGMAP_NAME` / `CA_BUNDLE_VOLUME_MOUNT_POINT` env vars.
- `python/storage/kserve_storage/kserve_storage.py` — reads the mounted cert at runtime
  and passes it to the storage client.

**When is `global-ca-bundle` created?** The `CaBundleConfigMapReconciler` in KServe runs
on **every InferenceService reconciliation**, regardless of storage URI scheme. The only
condition is whether `storageInitializer.caBundleConfigMapName` is set (non-empty) in the
global `inferenceservice-config` ConfigMap. If set, every user namespace with an
InferenceService gets a copy. The storage URI type is never checked.

**In the ODH/RHOAI setup, `global-ca-bundle` is NOT created.** The ODH operator patch
(`kserve/overlays/odh/patches/inferenceservice-config-patch.yaml`) does **not** set
`storageInitializer.caBundleConfigMapName`, so the global `CaBundleConfigMapReconciler`
path is inactive. Instead, ODH uses a **per-data-connection path**:

1. `secret_controller.go` (this package) builds the `storage-config` Secret from data
   connection secrets.
2. If `odh-kserve-custom-ca-bundle` has cert data **and** the data connection endpoint
   starts with `https://`, it sets `"cabundle_configmap": "odh-kserve-custom-ca-bundle"`
   in the storage-config JSON entry.
3. KServe's `service_account_credentials.go` reads that field and sets the
   `AWS_CA_BUNDLE_CONFIGMAP` env var on the storage-initializer container.
4. The Python storage initializer uses that env var for S3 downloads only.

This means the CA bundle in ODH only reaches the storage-initializer through the
`storage-config` Secret, and only for S3 data connections with HTTPS endpoints. The
`s3CABundleConfigMap` and `s3CABundle` fields in the `credentials` section of
`inferenceservice-config` serve as defaults for the S3 credential builder.

### Which Storage URIs Use the CA Bundle

| URI Scheme                        | CA Bundle Used | Notes                                                                                                                         |
|-----------------------------------|----------------|-------------------------------------------------------------------------------------------------------------------------------|
| `s3://`                           | Yes            | Passed to boto3 via `verify` kwarg. Controlled by `AWS_CA_BUNDLE` and `S3_VERIFY_SSL` env vars. This is the primary use case. |
| `hdfs://` / `webhdfs://`          | Partial        | Uses its own `TLS_CA` env var from HDFS credentials secret, **not** the global `cabundle.crt` mount.                          |
| `gs://`                           | No             | GCS client uses the system CA store only.                                                                                     |
| `https://*.blob.core.windows.net` | No             | Azure Blob SDK uses the system CA store only.                                                                                 |
| `https://*.file.core.windows.net` | No             | Azure File Share SDK uses the system CA store only.                                                                           |
| `hf://`                           | No             | Hugging Face `snapshot_download()` uses the system CA store only.                                                             |
| `http://` / `https://`            | No             | `requests.get()` called without `verify` parameter; uses system CA store.                                                     |
| `git` (https)                     | No             | `dulwich.porcelain.clone()` uses the system CA store only.                                                                    |
| `file://`                         | N/A            | Local filesystem, no network access.                                                                                          |

**Key takeaway:** The custom CA bundle is effectively only used for **S3-compatible
storage** (e.g., MinIO, Ceph with self-signed certs). All other providers fall back to
the system CA store and cannot use custom CA certificates injected through this pipeline.

### Workaround for non-S3 storage URIs (e.g., `hf://`)

In the ODH/RHOAI setup, the CA bundle file is **not mounted** into the storage-initializer
container for non-S3 URIs. The `global-ca-bundle` path is inactive (see above), and the
per-data-connection path only applies to S3. A full workaround requires two things:

1. **Mount the CA bundle ConfigMap as a volume** on the storage-initializer container.
2. **Set `REQUESTS_CA_BUNDLE`** env var to point at the mounted cert file.

The storage-initializer is an init container injected by the KServe webhook — it cannot
be customized directly through ServingRuntime specs. The available approaches are:

#### Option A: Custom `storage-initializer` init container in InferenceService (per-ISVC)

If the InferenceService pod spec already contains an init container named
`storage-initializer`, the KServe webhook **skips injection entirely**
(`storage_initializer_injector.go:299-302`). This gives full per-InferenceService control
over the init container, including volumes, env vars, and the CA bundle mount.

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: my-model
spec:
  predictor:
    model:
      storageUri: hf://my-org/my-model
    initContainers:
      - name: storage-initializer
        # Copy the full storage-initializer init container configuration (image, args, mounts, etc)
        image: <storage-initializer-image>
        args: ["hf://my-org/my-model", "/mnt/models"]
        env:
          # Add the REQUESTS_CA_BUNDLE 
          - name: REQUESTS_CA_BUNDLE
            value: /etc/ssl/custom-certs/cabundle.crt
          # Copy all other existing env vars ...
        volumeMounts:
          - name: ca-bundle
            mountPath: /etc/ssl/custom-certs
            readOnly: true
          - name: model-dir
            mountPath: /mnt/models
    volumes:
      - name: ca-bundle
        configMap:
          name: odh-kserve-custom-ca-bundle
      - name: model-dir
        emptyDir: {}
```

The `odh-kserve-custom-ca-bundle` ConfigMap **will exist** in the user namespace — the
ODH operator's `certconfigmapgenerator` controller creates `odh-trusted-ca-bundle` in all
namespaces (when DSCInitialization `trustedCABundle` is `Managed`), which triggers
`configmap_controller.go` to create `odh-kserve-custom-ca-bundle` in the same namespace.
The volume reference will resolve.

**Trade-offs:**
- Per-InferenceService control, no cluster-wide resources needed.
- **Bypasses all webhook logic** — credential injection (`storage-config` secret,
  service account credentials), HuggingFace optimization env vars, and S3 CA bundle
  configuration are all skipped. You must manually replicate everything the webhook
  would have done: image, args, resource limits, credential env vars, and volume mounts.
- Fragile across upgrades — the storage-initializer image and expected env vars may
  change between KServe versions.

#### Option B: Patch `inferenceservice-config` with `opendatahub.io/managed: "false"`

The ODH operator supports a per-resource annotation `opendatahub.io/managed: "false"`.
When set on a specific resource, the operator creates it if missing but **will not
overwrite** user changes on subsequent reconciliations. The rest of the KServe component
remains fully managed.

```bash
# 1. Annotate inferenceservice-config so the operator won't overwrite it
kubectl annotate configmap inferenceservice-config -n <kserve-namespace> \
  opendatahub.io/managed=false

# 2. Patch storageInitializer to set caBundleConfigMapName
#    (merge the new fields into the existing storageInitializer JSON)
kubectl get configmap inferenceservice-config -n <kserve-namespace> \
  -o jsonpath='{.data.storageInitializer}' > /tmp/si.json
# Edit /tmp/si.json to add:
#   "caBundleConfigMapName": "odh-kserve-custom-ca-bundle",
#   "caBundleVolumeMountPath": "/etc/ssl/custom-certs"
kubectl patch configmap inferenceservice-config -n <kserve-namespace> \
  --type merge -p "{\"data\":{\"storageInitializer\":$(cat /tmp/si.json | jq -c .| jq -Rs .)}}"
```

This activates KServe's `CaBundleConfigMapReconciler`, which copies
`odh-kserve-custom-ca-bundle` to each user namespace as `global-ca-bundle` and the
webhook automatically mounts it at `/etc/ssl/custom-certs/cabundle.crt` on the
storage-initializer container. No per-InferenceService changes needed for the volume mount.

**However**, this only solves half the problem — the CA bundle is now mounted, but non-S3
download code (e.g., `huggingface_hub`) still does not read it. You still need
`REQUESTS_CA_BUNDLE=/etc/ssl/custom-certs/cabundle.crt` set on the storage-initializer.
This could be done via a `ClusterStorageContainer` matching the relevant URI prefix.

**Trade-offs:**
- Solves the volume mount globally with no per-ISVC changes.
- KServe remains fully managed by the operator — only `inferenceservice-config` is
  excluded from reconciliation.
- User takes ownership of `inferenceservice-config` content — operator upgrades to
  that ConfigMap will not be applied automatically.
- Still requires a mechanism to inject `REQUESTS_CA_BUNDLE` (e.g., a
  `ClusterStorageContainer`).

#### Option C: Mutating webhook (fully automatic, no per-ISVC changes)

Deploy a custom mutating webhook that patches pods with the
`serving.kserve.io/inferenceservice` label to add:
- A volume from the `odh-kserve-custom-ca-bundle` ConfigMap.
- A volumeMount on the `storage-initializer` init container.
- The `REQUESTS_CA_BUNDLE` env var.

This is fully automatic and preserves the default webhook behavior (it runs **after**
KServe's webhook), but requires deploying and maintaining a webhook.

#### `REQUESTS_CA_BUNDLE` caveat

`REQUESTS_CA_BUNDLE` **replaces** the default system CA store — it does not append to it.
The `cabundle.crt` file must contain both the custom certs **and** the standard public
root CAs, otherwise requests to public endpoints (e.g., public Hugging Face Hub) will
fail. In most OpenShift clusters this is satisfied because `odh-trusted-ca-bundle`
includes the full system trust chain (injected via the
`config.openshift.io/inject-trusted-cabundle: "true"` label), but this should be verified
per cluster.

#### What would a product fix look like?

The ODH operator could set `storageInitializer.caBundleConfigMapName` in
`inferenceservice-config` to activate KServe's built-in `CaBundleConfigMapReconciler`.
This would mount the CA bundle on every storage-initializer automatically. Combined with
setting `REQUESTS_CA_BUNDLE` (e.g., via a kserve code change to the HF download path),
non-S3 providers would pick up custom CAs without any per-InferenceService configuration.