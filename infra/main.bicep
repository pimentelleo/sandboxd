targetScope = 'resourceGroup'

@description('Short, globally unique workload prefix. Resource names are derived from this value.')
param workloadPrefix string
@description('Brazil South is the supported production location.')
param location string = 'brazilsouth'
@description('CIDR assigned to the production virtual network.')
param vnetAddressPrefix string
@description('CIDR for AKS nodes.')
param aksSubnetPrefix string
@description('CIDR for private endpoints.')
param privateEndpointSubnetPrefix string
@description('CIDR for the delegated PostgreSQL subnet.')
param postgresSubnetPrefix string
@description('Resource ID of the pre-existing public Azure DNS zone.')
param dnsZoneResourceId string
@description('Microsoft Entra object ID of the production administrators group.')
param adminGroupObjectId string
@secure()
@description('Object ID of the protected deployment principal that runs the release pipeline.')
param deploymentPrincipalObjectId string
@description('PostgreSQL administrator login name. Do not use an Entra object ID.')
param postgresAdminLogin string
@secure()
@description('PostgreSQL administrator password supplied only by the deployment secret store.')
param postgresAdminPassword string
@description('AKS Kubernetes version approved for the target subscription.')
param kubernetesVersion string
@description('VM SKU validated for AKS Kata nested-virtualization support in Brazil South.')
param sandboxVmSize string
@description('VM SKU for the AKS system pool.')
param systemVmSize string = 'Standard_D4s_v5'
@description('Linux administrator username required by the AKS API; do not use a personal identity.')
param aksAdminUsername string
@description('SSH public key for AKS break-glass node access.')
param aksAdminSshPublicKey string
@description('Availability zone hosting the PostgreSQL primary.')
param postgresPrimaryZone string
@description('Different availability zone hosting PostgreSQL standby.')
param postgresStandbyZone string

var names = {
  acr: toLower('${workloadPrefix}acr')
  aks: '${workloadPrefix}-aks'
  keyVault: toLower('${workloadPrefix}-kv')
  postgres: toLower('${workloadPrefix}-pg')
  logAnalytics: '${workloadPrefix}-law'
  vnet: '${workloadPrefix}-vnet'
  ingressPublicIp: '${workloadPrefix}-ingress-pip'
  controlPlaneIdentity: '${workloadPrefix}-control-mi'
  certManagerIdentity: '${workloadPrefix}-cert-manager-mi'
}

module network 'modules/network.bicep' = {
  name: 'network'
  params: {
    location: location
    vnetName: names.vnet
    vnetAddressPrefix: vnetAddressPrefix
    aksSubnetPrefix: aksSubnetPrefix
    privateEndpointSubnetPrefix: privateEndpointSubnetPrefix
    postgresSubnetPrefix: postgresSubnetPrefix
    publicIpName: names.ingressPublicIp
  }
}

module monitoring 'modules/monitoring.bicep' = {
  name: 'monitoring'
  params: {
    location: location
    workspaceName: names.logAnalytics
  }
}

module keyVault 'modules/keyvault.bicep' = {
  name: 'key-vault'
  params: {
    location: location
    vaultName: names.keyVault
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    vnetId: network.outputs.vnetId
    logAnalyticsWorkspaceId: monitoring.outputs.workspaceId
  }
}

module postgres 'modules/postgres.bicep' = {
  name: 'postgres'
  params: {
    location: location
    serverName: names.postgres
    delegatedSubnetId: network.outputs.postgresSubnetId
    vnetId: network.outputs.vnetId
    administratorLogin: postgresAdminLogin
    administratorPassword: postgresAdminPassword
    primaryZone: postgresPrimaryZone
    standbyZone: postgresStandbyZone
    logAnalyticsWorkspaceId: monitoring.outputs.workspaceId
  }
}

module aks 'modules/aks.bicep' = {
  name: 'aks'
  params: {
    location: location
    clusterName: names.aks
    kubernetesVersion: kubernetesVersion
    adminGroupObjectId: adminGroupObjectId
    aksSubnetId: network.outputs.aksSubnetId
    systemVmSize: systemVmSize
    sandboxVmSize: sandboxVmSize
    adminUsername: aksAdminUsername
    adminSshPublicKey: aksAdminSshPublicKey
    logAnalyticsWorkspaceId: monitoring.outputs.workspaceId
  }
}

module registry 'modules/registry.bicep' = {
  name: 'registry'
  params: {
    location: location
    registryName: names.acr
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
    privateDnsZoneName: 'privatelink.azurecr.io'
    vnetId: network.outputs.vnetId
    kubeletObjectId: aks.outputs.kubeletObjectId
    deploymentPrincipalObjectId: deploymentPrincipalObjectId
    logAnalyticsWorkspaceId: monitoring.outputs.workspaceId
  }
}

module identities 'modules/identities.bicep' = {
  name: 'workload-identities'
  params: {
    location: location
    controlPlaneIdentityName: names.controlPlaneIdentity
    certManagerIdentityName: names.certManagerIdentity
    oidcIssuerUrl: aks.outputs.oidcIssuerUrl
  }
}

var dnsZoneSegments = split(dnsZoneResourceId, '/')

module dnsRole 'modules/dns-role.bicep' = {
  name: 'dns-zone-role'
  scope: resourceGroup(dnsZoneSegments[2], dnsZoneSegments[4])
  params: {
    dnsZoneName: dnsZoneSegments[8]
    certManagerPrincipalId: identities.outputs.certManagerPrincipalId
    deploymentPrincipalObjectId: deploymentPrincipalObjectId
    ingressPublicIp: network.outputs.publicIpAddress
  }
}

resource keyVaultResource 'Microsoft.KeyVault/vaults@2023-07-01' existing = {
  name: names.keyVault
}

resource keyVaultSecretsUser 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(keyVaultResource.id, 'sandboxd-control-plane-key-vault-secrets-user')
  scope: keyVaultResource
  properties: {
    principalId: identities.outputs.controlPlanePrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')
  }
}

resource keyVaultSecretsOfficer 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(keyVaultResource.id, 'sandboxd-deployment-principal-key-vault-secrets-officer')
  scope: keyVaultResource
  properties: {
    principalId: deploymentPrincipalObjectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b86a8fe4-44ce-4948-aee5-eccb2c155cd7')
  }
}

resource aksResource 'Microsoft.ContainerService/managedClusters@2024-09-01' existing = {
  name: names.aks
}

resource aksClusterUser 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksResource.id, 'sandboxd-deployment-principal-aks-cluster-user')
  scope: aksResource
  properties: {
    principalId: deploymentPrincipalObjectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4abbcc35-e782-43d8-92c5-2d3f1bd2253f')
  }
}

resource aksRbacClusterAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(aksResource.id, 'sandboxd-deployment-principal-aks-rbac-cluster-admin')
  scope: aksResource
  properties: {
    principalId: deploymentPrincipalObjectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b')
  }
}

resource ingressPublicIpResource 'Microsoft.Network/publicIPAddresses@2024-05-01' existing = {
  name: names.ingressPublicIp
}

resource aksNetworkContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(ingressPublicIpResource.id, 'sandboxd-aks-control-plane-network-contributor')
  scope: ingressPublicIpResource
  properties: {
    principalId: aks.outputs.controlPlanePrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b24988ac-6180-42a0-ab88-20f7382dd24c')
  }
}

output aksName string = names.aks
output aksResourceId string = aks.outputs.clusterId
output acrLoginServer string = registry.outputs.loginServer
output ingressPublicIp string = network.outputs.publicIpAddress
output keyVaultName string = names.keyVault
output postgresFqdn string = postgres.outputs.fqdn
output controlPlaneClientId string = identities.outputs.controlPlaneClientId
output certManagerClientId string = identities.outputs.certManagerClientId
