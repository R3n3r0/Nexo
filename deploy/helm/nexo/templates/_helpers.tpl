{{/*
Names. The full name is limited to 52 characters: the pods of the
StatefulSet are "<fullname>-<ordinal>" and the headless Service
"<fullname>-hl", both DNS labels.
*/}}
{{- define "nexo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 52 | trimSuffix "-" }}
{{- end }}

{{- define "nexo.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 52 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "nexo.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nexo.headlessName" -}}
{{- printf "%s-hl" (include "nexo.fullname" .) }}
{{- end }}

{{- define "nexo.labels" -}}
helm.sh/chart: {{ include "nexo.chart" . }}
{{ include "nexo.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: nexo
{{- end }}

{{- define "nexo.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nexo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Labels of the Helm test pods: never selected by the Services. */}}
{{- define "nexo.testLabels" -}}
helm.sh/chart: {{ include "nexo.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: helm-test
{{- end }}

{{- define "nexo.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nexo.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "nexo.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{- define "nexo.scheme" -}}
{{- if .Values.tls.enabled }}https{{ else }}http{{ end }}
{{- end }}

{{- define "nexo.probeScheme" -}}
{{- if .Values.tls.enabled }}HTTPS{{ else }}HTTP{{ end }}
{{- end }}

{{/* In-cluster address of the S3 Service. */}}
{{- define "nexo.serviceHost" -}}
{{- printf "%s.%s.svc.%s" (include "nexo.fullname" .) .Release.Namespace .Values.clusterDomain }}
{{- end }}

{{/* Secret holding the root credentials. */}}
{{- define "nexo.credentialsSecret" -}}
{{- default (include "nexo.fullname" .) .Values.auth.existingSecret }}
{{- end }}

{{- define "nexo.userKey" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.existingSecretUserKey }}{{ else }}root-user{{ end }}
{{- end }}

{{- define "nexo.passwordKey" -}}
{{- if .Values.auth.existingSecret }}{{ .Values.auth.existingSecretPasswordKey }}{{ else }}root-password{{ end }}
{{- end }}

{{/*
Root password of the chart Secret: the value, else the one of the Secret
already installed (kept on upgrades), else a random one.
*/}}
{{- define "nexo.rootPassword" -}}
{{- if .Values.auth.rootPassword }}
{{- .Values.auth.rootPassword }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "nexo.fullname" .) }}
{{- if and $existing (index ($existing.data | default dict) "root-password") }}
{{- index $existing.data "root-password" | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{- define "nexo.kmsEnabled" -}}
{{- if or .Values.kms.secretKey .Values.kms.existingSecret }}true{{ end }}
{{- end }}

{{- define "nexo.kmsSecret" -}}
{{- default (include "nexo.fullname" .) .Values.kms.existingSecret }}
{{- end }}

{{- define "nexo.kmsKey" -}}
{{- if .Values.kms.existingSecret }}{{ .Values.kms.existingSecretKey }}{{ else }}kms-secret-key{{ end }}
{{- end }}

{{/* Whether the chart creates its own Secret. */}}
{{- define "nexo.createSecret" -}}
{{- if or (not .Values.auth.existingSecret) (and .Values.kms.secretKey (not .Values.kms.existingSecret)) }}true{{ end }}
{{- end }}

{{/* Number of pods. */}}
{{- define "nexo.replicas" -}}
{{- if eq .Values.mode "distributed" }}
{{- mul (int .Values.distributed.replicas) (int .Values.distributed.pools) }}
{{- else }}1{{ end }}
{{- end }}

{{/* Number of drives of a pod. */}}
{{- define "nexo.drives" -}}
{{- if eq .Values.mode "distributed" }}{{ int .Values.distributed.drivesPerNode }}{{ else }}{{ int .Values.standalone.drives }}{{ end }}
{{- end }}

{{/* Mount path of drive i (1-based) of a pod. */}}
{{- define "nexo.drivePath" -}}
{{- if and (eq .root.Values.mode "standalone") (eq (int .root.Values.standalone.drives) 1) }}/data{{ else }}/data{{ .i }}{{ end }}
{{- end }}

{{/* Name of the volume of drive i (1-based). */}}
{{- define "nexo.driveVolume" -}}
{{- if and (eq .root.Values.mode "standalone") (eq (int .root.Values.standalone.drives) 1) }}data{{ else }}data{{ .i }}{{ end }}
{{- end }}

{{/*
Endpoints of "nexo server", one argument per pool:
  standalone: /data or /data{1...N}
  distributed: <scheme>://<fullname>-{a...b}.<headless>.<ns>.svc.<domain>:9000/data{1...M}
The nodes recognize their own endpoints through /etc/hosts (the pod FQDN
of the StatefulSet), the others through the headless Service, which
publishes the pods before they are ready.
*/}}
{{- define "nexo.endpoints" -}}
{{- $drives := int (include "nexo.drives" .) }}
{{- $paths := "/data" }}
{{- if gt $drives 1 }}{{ $paths = printf "/data{1...%d}" $drives }}{{ else if eq .Values.mode "distributed" }}{{ $paths = "/data1" }}{{ end }}
{{- if eq .Values.mode "distributed" }}
{{- $replicas := int .Values.distributed.replicas }}
{{- $fullname := include "nexo.fullname" . }}
{{- $domain := printf "%s.%s.svc.%s" (include "nexo.headlessName" .) .Release.Namespace .Values.clusterDomain }}
{{- $list := list }}
{{- range $pool := until (int .Values.distributed.pools) }}
{{- $first := mul $pool $replicas }}
{{- $last := sub (add $first $replicas) 1 }}
{{- $list = append $list (printf "%s://%s-{%d...%d}.%s:9000%s" (include "nexo.scheme" $) $fullname $first $last $domain $paths) }}
{{- end }}
{{- join " " $list }}
{{- else }}
{{- $paths }}
{{- end }}
{{- end }}

{{/*
"true" when an erasure set size in [2..16] divides the drives of a pool
and spreads evenly on its nodes (internal/service/topology/sets.go).
*/}}
{{- define "nexo.validSets" -}}
{{- $ok := false }}
{{- range $s := untilStep 2 17 1 }}
{{- if eq (mod $.total $s) 0 }}
{{- if ge $s $.nodes }}
{{- if eq (mod $s $.nodes) 0 }}{{ $ok = true }}{{ end }}
{{- else if eq (mod $.nodes $s) 0 }}{{ $ok = true }}
{{- end }}
{{- end }}
{{- end }}
{{- if $ok }}true{{ end }}
{{- end }}

{{/* Fails the rendering on an invalid combination of values. */}}
{{- define "nexo.validate" -}}
{{- if not (has .Values.mode (list "standalone" "distributed")) }}
{{- fail (printf "mode must be standalone or distributed, not %q" .Values.mode) }}
{{- end }}
{{- if and (not .Values.auth.existingSecret) (lt (len .Values.auth.rootUser) 3) }}
{{- fail "auth.rootUser must have at least 3 characters" }}
{{- end }}
{{- if and (not .Values.auth.existingSecret) .Values.auth.rootPassword (lt (len .Values.auth.rootPassword) 8) }}
{{- fail "auth.rootPassword must have at least 8 characters (or be empty to generate one)" }}
{{- end }}
{{- if and .Values.tls.enabled (not .Values.tls.secretName) }}
{{- fail "tls.enabled requires tls.secretName (a kubernetes.io/tls Secret)" }}
{{- end }}
{{- if and .Values.kms.kes.endpoint (include "nexo.kmsEnabled" .) }}
{{- fail "kms.kes.endpoint and the builtin kms.secretKey/existingSecret are exclusive" }}
{{- end }}
{{- if and .Values.kms.kes.endpoint (not .Values.kms.kes.existingSecret) }}
{{- fail "kms.kes.endpoint requires kms.kes.existingSecret (client.crt, client.key, ca.crt)" }}
{{- end }}
{{- if eq .Values.mode "distributed" }}
{{- $nodes := int .Values.distributed.replicas }}
{{- $drives := int .Values.distributed.drivesPerNode }}
{{- if lt $nodes 2 }}
{{- fail "distributed.replicas must be at least 2 (use mode=standalone for one pod)" }}
{{- end }}
{{- if lt $drives 1 }}
{{- fail "distributed.drivesPerNode must be at least 1" }}
{{- end }}
{{- if lt (int .Values.distributed.pools) 1 }}
{{- fail "distributed.pools must be at least 1" }}
{{- end }}
{{- if not (include "nexo.validSets" (dict "total" (mul $nodes $drives) "nodes" $nodes)) }}
{{- fail (printf "no erasure set of 2 to 16 drives fits %d drives on %d nodes: change distributed.replicas or distributed.drivesPerNode" (mul $nodes $drives) $nodes) }}
{{- end }}
{{- else }}
{{- $drives := int .Values.standalone.drives }}
{{- if lt $drives 1 }}
{{- fail "standalone.drives must be at least 1" }}
{{- end }}
{{- if and (gt $drives 1) (not (include "nexo.validSets" (dict "total" $drives "nodes" 1))) }}
{{- fail (printf "no erasure set of 2 to 16 drives divides %d drives: change standalone.drives" $drives) }}
{{- end }}
{{- if and .Values.persistence.existingClaim (gt $drives 1) }}
{{- fail "persistence.existingClaim is supported with standalone.drives=1 only" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Public URL from the first host of an Ingress. */}}
{{- define "nexo.ingressURL" -}}
{{- $ing := . }}
{{- if and $ing.enabled $ing.hosts }}
{{- printf "%s://%s" (ternary "https" "http" (gt (len $ing.tls) 0)) (first $ing.hosts) }}
{{- end }}
{{- end }}

{{- define "nexo.storageClass" -}}
{{- if . }}
{{- if eq "-" . }}
storageClassName: ""
{{- else }}
storageClassName: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Media worker. Its pods carry their own app.kubernetes.io/name, so that the
Services of Nexo never select them.
*/}}
{{- define "nexo.mediaWorker.fullname" -}}
{{- printf "%s-media-worker" (include "nexo.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nexo.mediaWorker.selectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-media-worker" (include "nexo.name" .) | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: media-worker
{{- end }}

{{- define "nexo.mediaWorker.labels" -}}
helm.sh/chart: {{ include "nexo.chart" . }}
{{ include "nexo.mediaWorker.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: nexo
{{- end }}

{{/* Labels of the setup Job pods (never selected by a Service). */}}
{{- define "nexo.mediaWorker.setupLabels" -}}
helm.sh/chart: {{ include "nexo.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: nexo
app.kubernetes.io/component: media-worker-setup
{{- end }}

{{- define "nexo.mediaWorker.image" -}}
{{- $img := .Values.mediaWorker.image }}
{{- if $img.digest }}
{{- printf "%s@%s" $img.repository $img.digest }}
{{- else }}
{{- printf "%s:%s" $img.repository (default .Chart.AppVersion $img.tag) }}
{{- end }}
{{- end }}

{{/* Secret with access-key, secret-key and auth-token of the worker. */}}
{{- define "nexo.mediaWorker.secret" -}}
{{- default (include "nexo.mediaWorker.fullname" .) .Values.mediaWorker.auth.existingSecret }}
{{- end }}

{{/*
A generated value of the worker Secret: the value, else the one of the
Secret already installed (kept on upgrades), else a random one.
*/}}
{{- define "nexo.mediaWorker.secretValue" -}}
{{- $root := .root }}
{{- if .value }}
{{- .value }}
{{- else }}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace (include "nexo.mediaWorker.fullname" $root) }}
{{- if and $existing (index ($existing.data | default dict) .key) }}
{{- index $existing.data .key | b64dec }}
{{- else }}
{{- randAlphaNum 40 }}
{{- end }}
{{- end }}
{{- end }}

{{/* S3 URL and Admin API URL of Nexo, through its Service. */}}
{{- define "nexo.s3URL" -}}
{{- printf "%s://%s:%v" (include "nexo.scheme" .) (include "nexo.serviceHost" .) .Values.service.s3Port }}
{{- end }}

{{- define "nexo.adminURL" -}}
{{- printf "%s://%s:%v" (include "nexo.scheme" .) (include "nexo.serviceHost" .) .Values.service.consolePort }}
{{- end }}

{{/* Jobs endpoint of the worker (endpoint of the notify_media_worker target). */}}
{{- define "nexo.mediaWorker.endpoint" -}}
{{- printf "http://%s.%s.svc.%s:%v/jobs" (include "nexo.mediaWorker.fullname" .) .Release.Namespace .Values.clusterDomain .Values.mediaWorker.service.port }}
{{- end }}

{{/*
Least-privilege policy of the worker (docs/user/media-worker.md): it reads
the objects and writes only under .nexo-media/ of mediaWorker.setup.buckets
(every bucket when empty).
*/}}
{{- define "nexo.mediaWorker.policy" -}}
{{- $buckets := .Values.mediaWorker.setup.buckets | default (list "*") }}
{{- $objects := list }}
{{- $media := list }}
{{- $arns := list }}
{{- range $buckets }}
{{- $objects = append $objects (printf "arn:aws:s3:::%s/*" .) }}
{{- $media = append $media (printf "arn:aws:s3:::%s/.nexo-media/*" .) }}
{{- $arns = append $arns (printf "arn:aws:s3:::%s" .) }}
{{- end }}
{{- $statements := list
  (dict "Effect" "Allow" "Action" (list "s3:GetObject" "s3:GetObjectVersion" "nexo:DownloadProtectedMedia") "Resource" $objects)
  (dict "Effect" "Allow" "Action" (list "s3:PutObject" "s3:DeleteObject" "s3:DeleteObjectVersion") "Resource" $media)
  (dict "Effect" "Allow" "Action" (list "s3:ListBucket" "s3:ListBucketVersions") "Resource" $arns
    "Condition" (dict "StringLike" (dict "s3:prefix" (list ".nexo-media/*"))))
}}
{{- dict "Version" "2012-10-17" "Statement" $statements | toPrettyJson }}
{{- end }}

{{/* Fails the rendering on invalid values of the media worker. */}}
{{- define "nexo.mediaWorker.validate" -}}
{{- $mw := .Values.mediaWorker }}
{{- if gt (int $mw.replicas) 1 }}
{{- fail "mediaWorker.replicas must be 0 or 1: every worker has its own job queue, scale it up (resources, concurrency) instead" }}
{{- end }}
{{- if not $mw.auth.existingSecret }}
{{- if lt (len $mw.auth.accessKey) 3 }}
{{- fail "mediaWorker.auth.accessKey must have at least 3 characters" }}
{{- end }}
{{- if and $mw.auth.secretKey (lt (len $mw.auth.secretKey) 8) }}
{{- fail "mediaWorker.auth.secretKey must have at least 8 characters (or be empty to generate one)" }}
{{- end }}
{{- end }}
{{- if not (regexMatch "^notify_media_worker(:[A-Za-z0-9_-]+)?$" $mw.setup.target) }}
{{- fail (printf "mediaWorker.setup.target must be notify_media_worker or notify_media_worker:<id>, not %q" $mw.setup.target) }}
{{- end }}
{{- if and $mw.persistence.existingClaim (not $mw.persistence.enabled) }}
{{- fail "mediaWorker.persistence.existingClaim requires mediaWorker.persistence.enabled" }}
{{- end }}
{{- end }}
