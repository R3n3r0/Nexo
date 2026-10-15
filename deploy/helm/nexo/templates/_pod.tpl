{{/*
Pod template shared by the standalone Deployment and the distributed
StatefulSet.
*/}}
{{- define "nexo.podTemplate" -}}
{{- $root := . -}}
metadata:
  labels:
    {{- include "nexo.selectorLabels" . | nindent 4 }}
    app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
    app.kubernetes.io/part-of: nexo
    {{- with .Values.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- if or (include "nexo.createSecret" .) .Values.podAnnotations }}
  annotations:
    {{- if include "nexo.createSecret" . }}
    checksum/secret: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}
    {{- end }}
    {{- with .Values.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- end }}
spec:
  serviceAccountName: {{ include "nexo.serviceAccountName" . }}
  automountServiceAccountToken: {{ .Values.serviceAccount.automountServiceAccountToken }}
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.priorityClassName }}
  priorityClassName: {{ . }}
  {{- end }}
  terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  {{- if or .Values.affinity (and (eq .Values.mode "distributed") .Values.podAntiAffinity) }}
  affinity:
    {{- if .Values.affinity }}
    {{- toYaml .Values.affinity | nindent 4 }}
    {{- else if eq .Values.podAntiAffinity "hard" }}
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyKey: {{ .Values.podAntiAffinityTopologyKey }}
          labelSelector:
            matchLabels:
              {{- include "nexo.selectorLabels" . | nindent 14 }}
    {{- else if eq .Values.podAntiAffinity "soft" }}
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: {{ .Values.podAntiAffinityTopologyKey }}
            labelSelector:
              matchLabels:
                {{- include "nexo.selectorLabels" . | nindent 16 }}
    {{- end }}
  {{- end }}
  {{- with .Values.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: nexo
      image: {{ include "nexo.image" . | quote }}
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args:
        - server
        {{- range .Values.extraArgs }}
        - {{ . | quote }}
        {{- end }}
        {{- range splitList " " (include "nexo.endpoints" .) }}
        - {{ . | quote }}
        {{- end }}
      env:
        - name: NEXO_ADDRESS
          value: ":9000"
        - name: NEXO_CONSOLE_ADDRESS
          value: ":9001"
        - name: NEXO_ROOT_USER_FILE
          value: /run/secrets/nexo/root-user
        - name: NEXO_ROOT_PASSWORD_FILE
          value: /run/secrets/nexo/root-password
        {{- if include "nexo.kmsEnabled" . }}
        - name: NEXO_KMS_SECRET_KEY_FILE
          value: /run/secrets/nexo/kms-secret-key
        {{- end }}
        {{- with .Values.kms.kes.endpoint }}
        - name: NEXO_KMS_ENDPOINT
          value: {{ . | quote }}
        - name: NEXO_KMS_CLIENT_CERT
          value: /etc/nexo/kes/client.crt
        - name: NEXO_KMS_CLIENT_KEY
          value: /etc/nexo/kes/client.key
        - name: NEXO_KMS_CAPATH
          value: /etc/nexo/kes/ca.crt
        {{- end }}
        {{- with .Values.kms.kes.keyName }}
        - name: NEXO_KMS_KEY_NAME
          value: {{ . | quote }}
        {{- end }}
        {{- if .Values.tls.enabled }}
        - name: NEXO_CERTS_DIR
          value: /etc/nexo/certs
        {{- with .Values.tls.minVersion }}
        - name: NEXO_TLS_MIN_VERSION
          value: {{ . | quote }}
        {{- end }}
        {{- if .Values.tls.internodeVerify }}
        - name: NEXO_TLS_INTERNODE_VERIFY
          value: "true"
        {{- end }}
        {{- end }}
        {{- if .Values.cache.enabled }}
        - name: NEXO_CACHE_DIR
          value: /cache
        {{- with .Values.cache.size }}
        - name: NEXO_CACHE_SIZE
          value: {{ . | quote }}
        {{- end }}
        {{- end }}
        {{- with .Values.cache.memory }}
        - name: NEXO_CACHE_MEMORY
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.cache.mode }}
        - name: NEXO_CACHE_MODE
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.region }}
        - name: NEXO_REGION
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.domain }}
        - name: NEXO_DOMAIN
          value: {{ . | quote }}
        {{- end }}
        {{- with (default (include "nexo.ingressURL" .Values.ingress.s3) .Values.config.serverURL) }}
        - name: NEXO_SERVER_URL
          value: {{ . | quote }}
        {{- end }}
        {{- with (default (include "nexo.ingressURL" .Values.ingress.console) .Values.config.browserRedirectURL) }}
        - name: NEXO_BROWSER_REDIRECT_URL
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.trustedProxies }}
        - name: NEXO_TRUSTED_PROXIES
          value: {{ join "," . | quote }}
        {{- end }}
        {{- with .Values.config.forwardedHeaders }}
        - name: NEXO_FORWARDED_HEADERS
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.logLevel }}
        - name: NEXO_LOG_LEVEL
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.logFormat }}
        - name: NEXO_LOG_FORMAT
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.prometheusAuthType }}
        - name: NEXO_PROMETHEUS_AUTH_TYPE
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.storageClassStandard }}
        - name: NEXO_STORAGE_CLASS_STANDARD
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.storageClassRRS }}
        - name: NEXO_STORAGE_CLASS_RRS
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.otel.endpoint }}
        - name: NEXO_OTEL_EXPORTER_OTLP_ENDPOINT
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.otel.protocol }}
        - name: NEXO_OTEL_EXPORTER_OTLP_PROTOCOL
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.config.otel.sampleRatio }}
        - name: NEXO_OTEL_TRACES_SAMPLER_ARG
          value: {{ . | quote }}
        {{- end }}
        {{- with .Values.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with .Values.envFrom }}
      envFrom:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      ports:
        - name: s3
          containerPort: 9000
          protocol: TCP
        - name: console
          containerPort: 9001
          protocol: TCP
      {{- if .Values.probes.startup.enabled }}
      startupProbe:
        httpGet:
          path: /nexo/health/live
          port: s3
          scheme: {{ include "nexo.probeScheme" . }}
        periodSeconds: {{ .Values.probes.startup.periodSeconds }}
        timeoutSeconds: {{ .Values.probes.startup.timeoutSeconds }}
        failureThreshold: {{ .Values.probes.startup.failureThreshold }}
      {{- end }}
      {{- if .Values.probes.liveness.enabled }}
      livenessProbe:
        httpGet:
          path: /nexo/health/live
          port: s3
          scheme: {{ include "nexo.probeScheme" . }}
        periodSeconds: {{ .Values.probes.liveness.periodSeconds }}
        timeoutSeconds: {{ .Values.probes.liveness.timeoutSeconds }}
        failureThreshold: {{ .Values.probes.liveness.failureThreshold }}
      {{- end }}
      {{- if .Values.probes.readiness.enabled }}
      readinessProbe:
        httpGet:
          path: /nexo/health/ready
          port: s3
          scheme: {{ include "nexo.probeScheme" . }}
        periodSeconds: {{ .Values.probes.readiness.periodSeconds }}
        timeoutSeconds: {{ .Values.probes.readiness.timeoutSeconds }}
        failureThreshold: {{ .Values.probes.readiness.failureThreshold }}
      {{- end }}
      resources:
        {{- toYaml .Values.resources | nindent 8 }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      volumeMounts:
        {{- range $i := until (int (include "nexo.drives" .)) }}
        - name: {{ include "nexo.driveVolume" (dict "root" $root "i" (add1 $i)) }}
          mountPath: {{ include "nexo.drivePath" (dict "root" $root "i" (add1 $i)) }}
        {{- end }}
        - name: secrets
          mountPath: /run/secrets/nexo
          readOnly: true
        - name: tmp
          mountPath: /tmp
        {{- if .Values.tls.enabled }}
        - name: certs
          mountPath: /etc/nexo/certs
          readOnly: true
        {{- end }}
        {{- if .Values.kms.kes.endpoint }}
        - name: kes
          mountPath: /etc/nexo/kes
          readOnly: true
        {{- end }}
        {{- if .Values.cache.enabled }}
        - name: cache
          mountPath: /cache
        {{- end }}
  volumes:
    - name: secrets
      projected:
        defaultMode: 0440
        sources:
          - secret:
              name: {{ include "nexo.credentialsSecret" . }}
              items:
                - key: {{ include "nexo.userKey" . }}
                  path: root-user
                - key: {{ include "nexo.passwordKey" . }}
                  path: root-password
          {{- if include "nexo.kmsEnabled" . }}
          - secret:
              name: {{ include "nexo.kmsSecret" . }}
              items:
                - key: {{ include "nexo.kmsKey" . }}
                  path: kms-secret-key
          {{- end }}
    - name: tmp
      emptyDir: {}
    {{- if .Values.tls.enabled }}
    - name: certs
      projected:
        defaultMode: 0440
        sources:
          - secret:
              name: {{ .Values.tls.secretName }}
              items:
                - key: tls.crt
                  path: public.crt
                - key: tls.key
                  path: private.key
          {{- with .Values.tls.caSecretName }}
          - secret:
              name: {{ . }}
              items:
                - key: {{ $.Values.tls.caKey }}
                  path: CAs/ca.crt
          {{- end }}
    {{- end }}
    {{- if .Values.kms.kes.endpoint }}
    - name: kes
      secret:
        secretName: {{ .Values.kms.kes.existingSecret }}
        defaultMode: 0440
    {{- end }}
    {{- if and .Values.cache.enabled (eq .Values.cache.volume.type "emptyDir") }}
    - name: cache
      emptyDir:
        {{- with .Values.cache.volume.medium }}
        medium: {{ . }}
        {{- end }}
        {{- with .Values.cache.volume.size }}
        sizeLimit: {{ . }}
        {{- end }}
    {{- else if and .Values.cache.enabled (eq .Values.mode "standalone") }}
    - name: cache
      persistentVolumeClaim:
        claimName: {{ include "nexo.fullname" . }}-cache
    {{- end }}
    {{- if eq .Values.mode "standalone" }}
    {{- range $i := until (int .Values.standalone.drives) }}
    - name: {{ include "nexo.driveVolume" (dict "root" $root "i" (add1 $i)) }}
      {{- if not $root.Values.persistence.enabled }}
      emptyDir: {}
      {{- else if $root.Values.persistence.existingClaim }}
      persistentVolumeClaim:
        claimName: {{ $root.Values.persistence.existingClaim }}
      {{- else }}
      persistentVolumeClaim:
        claimName: {{ include "nexo.fullname" $root }}-{{ include "nexo.driveVolume" (dict "root" $root "i" (add1 $i)) }}
      {{- end }}
    {{- end }}
    {{- else if not .Values.persistence.enabled }}
    {{- range $i := until (int .Values.distributed.drivesPerNode) }}
    - name: data{{ add1 $i }}
      emptyDir: {}
    {{- end }}
    {{- end }}
{{- end }}
