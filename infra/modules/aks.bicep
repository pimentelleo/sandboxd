param location string
param clusterName string
param kubernetesVersion string
param adminGroupObjectId string
param aksSubnetId string
param systemVmSize string
param sandboxVmSize string
param adminUsername string
param adminSshPublicKey string
param logAnalyticsWorkspaceId string

resource cluster 'Microsoft.ContainerService/managedClusters@2024-09-01' = {
  name: clusterName
  location: location
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    dnsPrefix: toLower(clusterName)
    kubernetesVersion: kubernetesVersion
    enableRBAC: true
    disableLocalAccounts: true
    apiServerAccessProfile: {
      enablePrivateCluster: true
      privateDNSZone: 'system'
    }
    aadProfile: {
      managed: true
      enableAzureRBAC: true
      adminGroupObjectIDs: [adminGroupObjectId]
    }
    oidcIssuerProfile: {
      enabled: true
    }
    securityProfile: {
      workloadIdentity: {
        enabled: true
      }
    }
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkDataplane: 'cilium'
      networkPolicy: 'cilium'
      loadBalancerSku: 'standard'
      outboundType: 'loadBalancer'
    }
    addonProfiles: {
      azureKeyvaultSecretsProvider: {
        enabled: true
      }
      azureDiskCSIDriver: {
        enabled: true
      }
      azureBlobCSIDriver: {
        enabled: false
      }
    }
    agentPoolProfiles: [
      {
        name: 'system'
        mode: 'System'
        count: 3
        vmSize: systemVmSize
        osType: 'Linux'
        osSKU: 'AzureLinux'
        type: 'VirtualMachineScaleSets'
        availabilityZones: ['1', '2', '3']
        enableAutoScaling: true
        minCount: 3
        maxCount: 6
        vnetSubnetID: aksSubnetId
      }
    ]
    linuxProfile: {
      adminUsername: adminUsername
      ssh: {
        publicKeys: [
          {
            keyData: adminSshPublicKey
          }
        ]
      }
    }
  }
}

resource sandboxPool 'Microsoft.ContainerService/managedClusters/agentPools@2024-09-01' = {
  parent: cluster
  name: 'sandbox'
  properties: {
    mode: 'User'
    count: 0
    vmSize: sandboxVmSize
    osType: 'Linux'
    osSKU: 'AzureLinux'
    type: 'VirtualMachineScaleSets'
    availabilityZones: ['1', '2', '3']
    enableAutoScaling: true
    minCount: 0
    maxCount: 20
    vnetSubnetID: aksSubnetId
    nodeTaints: [
      'sandboxd.io/isolation=kata:NoSchedule'
    ]
    nodeLabels: {
      'sandboxd.io/isolation': 'kata'
    }
    workloadRuntime: 'KataMshvVmIsolation'
  }
}

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  scope: cluster
  name: 'to-log-analytics'
  properties: {
    workspaceId: logAnalyticsWorkspaceId
    logs: [
      {
        categoryGroup: 'allLogs'
        enabled: true
      }
    ]
    metrics: [
      {
        category: 'AllMetrics'
        enabled: true
      }
    ]
  }
}

output clusterId string = cluster.id
output oidcIssuerUrl string = cluster.properties.oidcIssuerProfile.issuerURL
output kubeletObjectId string = cluster.properties.identityProfile.kubeletidentity.objectId
output controlPlanePrincipalId string = cluster.identity.principalId
