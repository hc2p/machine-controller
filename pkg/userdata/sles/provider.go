/*
Copyright 2019 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

//
// UserData plugin for SLES.
//

package sles

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"github.com/Masterminds/semver/v3"

	"github.com/kubermatic/machine-controller/pkg/apis/plugin"
	providerconfigtypes "github.com/kubermatic/machine-controller/pkg/providerconfig/types"
	userdatahelper "github.com/kubermatic/machine-controller/pkg/userdata/helper"
)

// Provider is a pkg/userdata/plugin.Provider implementation.
type Provider struct{}

// UserData renders user-data template to string.
func (p Provider) UserData(req plugin.UserDataRequest) (string, error) {
	tmpl, err := template.New("user-data").Funcs(userdatahelper.TxtFuncMap()).Parse(userDataTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse user-data template: %w", err)
	}

	kubeletVersion, err := semver.NewVersion(req.MachineSpec.Versions.Kubelet)
	if err != nil {
		return "", fmt.Errorf("invalid kubelet version: %w", err)
	}

	pconfig, err := providerconfigtypes.GetConfig(req.MachineSpec.ProviderSpec)
	if err != nil {
		return "", fmt.Errorf("failed to get providerSpec: %w", err)
	}

	if pconfig.OverwriteCloudConfig != nil {
		req.CloudConfig = *pconfig.OverwriteCloudConfig
	}

	if pconfig.Network.IsStaticIPConfig() {
		return "", errors.New("static IP config is not supported with SLES")
	}

	slesConfig, err := LoadConfig(pconfig.OperatingSystemSpec)
	if err != nil {
		return "", fmt.Errorf("failed to get sles config from provider config: %w", err)
	}

	serverAddr, err := userdatahelper.GetServerAddressFromKubeconfig(req.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("error extracting server address from kubeconfig: %w", err)
	}

	kubeconfigString, err := userdatahelper.StringifyKubeconfig(req.Kubeconfig)
	if err != nil {
		return "", err
	}

	kubernetesCACert, err := userdatahelper.GetCACert(req.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("error extracting cacert: %w", err)
	}

	crEngine := req.ContainerRuntime.Engine(kubeletVersion)
	crConfig, err := crEngine.Config()
	if err != nil {
		return "", fmt.Errorf("failed to generate container runtime config: %w", err)
	}

	crAuthConfig, err := crEngine.AuthConfig()
	if err != nil {
		return "", fmt.Errorf("failed to generate container runtime auth config: %w", err)
	}

	data := struct {
		plugin.UserDataRequest
		ProviderSpec                       *providerconfigtypes.Config
		OSConfig                           *Config
		ServerAddr                         string
		KubeletVersion                     string
		Kubeconfig                         string
		KubernetesCACert                   string
		NodeIPScript                       string
		ExtraKubeletFlags                  []string
		ContainerRuntimeConfigFileName     string
		ContainerRuntimeConfig             string
		ContainerRuntimeAuthConfigFileName string
		ContainerRuntimeAuthConfig         string
		ContainerRuntimeName               string
	}{
		UserDataRequest:                    req,
		ProviderSpec:                       pconfig,
		OSConfig:                           slesConfig,
		ServerAddr:                         serverAddr,
		KubeletVersion:                     kubeletVersion.String(),
		Kubeconfig:                         kubeconfigString,
		KubernetesCACert:                   kubernetesCACert,
		NodeIPScript:                       userdatahelper.SetupNodeIPEnvScript(),
		ExtraKubeletFlags:                  crEngine.KubeletFlags(),
		ContainerRuntimeConfigFileName:     crEngine.ConfigFileName(),
		ContainerRuntimeConfig:             crConfig,
		ContainerRuntimeAuthConfigFileName: crEngine.AuthConfigFileName(),
		ContainerRuntimeAuthConfig:         crAuthConfig,
		ContainerRuntimeName:               crEngine.String(),
	}
	b := &bytes.Buffer{}
	err = tmpl.Execute(b, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute user-data template: %w", err)
	}
	return userdatahelper.CleanupTemplateOutput(b.String())
}

// UserData template.
const userDataTemplate = `#cloud-config
{{ if ne .CloudProviderName "aws" }}
hostname: {{ .MachineSpec.Name }}
{{- /* Never set the hostname on AWS nodes. Kubernetes(kube-proxy) requires the hostname to be the private dns name */}}
{{ end }}

{{- if .OSConfig.DistUpgradeOnBoot }}
package_upgrade: true
package_reboot_if_required: true
{{- end }}

ssh_pwauth: false

{{- if .ProviderSpec.SSHPublicKeys }}
ssh_authorized_keys:
{{- range .ProviderSpec.SSHPublicKeys }}
- "{{ . }}"
{{- end }}
{{- end }}

write_files:
{{- if .HTTPProxy }}
- path: "/etc/environment"
  content: |
    PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games"
{{ proxyEnvironment .HTTPProxy .NoProxy | indent 4 }}
{{- end }}

- path: "/etc/systemd/journald.conf.d/max_disk_use.conf"
  content: |
{{ journalDConfig | indent 4 }}

- path: "/opt/load-kernel-modules.sh"
  permissions: "0755"
  content: |
{{ kernelModulesScript | indent 4 }}

- path: "/etc/sysctl.d/k8s.conf"
  content: |
{{ kernelSettings | indent 4 }}

- path: "/opt/bin/setup"
  permissions: "0755"
  content: |
    #!/bin/bash
    set -xeuo pipefail
{{- /* As we added some modules and don't want to reboot, restart the service */}}
    systemctl restart systemd-modules-load.service
    sysctl --system

    zypper --non-interactive --quiet --color install ebtables \
      ceph-common \
      e2fsprogs \
      jq \
      socat \
      {{- if or (eq .CloudProviderName "vsphere") (eq .CloudProviderName "vmware-cloud-director") }}
      open-vm-tools \
      {{- end }}
      ipvsadm

{{ safeDownloadBinariesScript .KubeletVersion | indent 4 }}

    # set kubelet nodeip environment variable
    /opt/bin/setup_net_env.sh

    systemctl enable --now docker
    systemctl enable --now kubelet
    systemctl enable --now --no-block kubelet-healthcheck.service
    systemctl enable --now --no-block docker-healthcheck.service

- path: "/opt/bin/supervise.sh"
  permissions: "0755"
  content: |
    #!/bin/bash
    set -xeuo pipefail
    while ! "$@"; do
      sleep 1
    done

- path: "/opt/disable-swap.sh"
  permissions: "0755"
  content: |
    # Make sure we always disable swap - Otherwise the kubelet won't start as for some cloud
    # providers swap gets enabled on reboot or after the setup script has finished executing.
    cp /etc/fstab /etc/fstab.orig
    cat /etc/fstab.orig | awk '$3 ~ /^swap$/ && $1 !~ /^#/ {$0="# commented out by cloudinit\n#"$0} 1' > /etc/fstab.noswap
    mv /etc/fstab.noswap /etc/fstab
    swapoff -a

- path: "/etc/systemd/system/kubelet.service"
  content: |
{{ kubeletSystemdUnit .ContainerRuntimeName .KubeletVersion .KubeletCloudProviderName .MachineSpec.Name .DNSIPs .ExternalCloudProvider .PauseImage .MachineSpec.Taints .ExtraKubeletFlags true | indent 4 }}

- path: "/etc/systemd/system/kubelet.service.d/extras.conf"
  content: |
    [Service]
    Environment="KUBELET_EXTRA_ARGS=--resolv-conf=/var/run/netconfig/resolv.conf"

- path: "/etc/kubernetes/cloud-config"
  permissions: "0600"
  content: |
{{ .CloudConfig | indent 4 }}

- path: "/opt/bin/setup_net_env.sh"
  permissions: "0755"
  content: |
{{ .NodeIPScript | indent 4 }}

- path: "/etc/kubernetes/bootstrap-kubelet.conf"
  permissions: "0600"
  content: |
{{ .Kubeconfig | indent 4 }}

- path: "/etc/kubernetes/pki/ca.crt"
  content: |
{{ .KubernetesCACert | indent 4 }}

- path: "/etc/systemd/system/setup.service"
  permissions: "0644"
  content: |
    [Install]
    WantedBy=multi-user.target

    [Unit]
    Requires=network-online.target
    After=network-online.target

    [Service]
    Type=oneshot
    RemainAfterExit=true
    EnvironmentFile=-/etc/environment
    ExecStart=/opt/bin/supervise.sh /opt/bin/setup

- path: "/etc/kubernetes/kubelet.conf"
  content: |
{{ kubeletConfiguration "cluster.local" .DNSIPs .KubeletFeatureGates .KubeletConfigs .ContainerRuntimeName | indent 4 }}

- path: "/etc/profile.d/opt-bin-path.sh"
  permissions: "0644"
  content: |
    export PATH="/opt/bin:$PATH"

- path: {{ .ContainerRuntimeConfigFileName }}
  permissions: "0644"
  content: |
{{ .ContainerRuntimeConfig | indent 4 }}

{{- if and (eq .ContainerRuntimeName "docker") .ContainerRuntimeAuthConfig }}

- path: {{ .ContainerRuntimeAuthConfigFileName }}
  permissions: "0600"
  content: |
{{ .ContainerRuntimeAuthConfig | indent 4 }}
{{- end }}

- path: /etc/systemd/system/kubelet-healthcheck.service
  permissions: "0644"
  content: |
{{ kubeletHealthCheckSystemdUnit | indent 4 }}

- path: /etc/systemd/system/docker-healthcheck.service
  permissions: "0644"
  content: |
{{ containerRuntimeHealthCheckSystemdUnit .ContainerRuntimeName | indent 4 }}

- path: /etc/systemd/system/docker.service.d/environment.conf
  permissions: "0644"
  content: |
    [Service]
    EnvironmentFile=-/etc/environment

{{- with .ProviderSpec.CAPublicKey }}

- path: "/etc/ssh/trusted-user-ca-keys.pem"
  content: |
{{ . | indent 4 }}

- path: "/etc/ssh/sshd_config"
  content: |
{{ sshConfigAddendum | indent 4 }}
  append: true
{{- end }}

runcmd:
- systemctl start setup.service
`
