param dnsZoneName string
param certManagerPrincipalId string
@secure()
param deploymentPrincipalObjectId string
param ingressPublicIp string

resource dnsZone 'Microsoft.Network/dnsZones@2023-07-01-preview' existing = {
  name: dnsZoneName
}

resource dnsZoneContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(dnsZone.id, 'sandboxd-cert-manager-dns-zone-contributor')
  scope: dnsZone
  properties: {
    principalId: certManagerPrincipalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'befefa01-2a29-4197-83a8-272ff33ce314')
  }
}

resource deploymentDnsZoneContributor 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(dnsZone.id, 'sandboxd-deployment-principal-dns-zone-contributor')
  scope: dnsZone
  properties: {
    principalId: deploymentPrincipalObjectId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'befefa01-2a29-4197-83a8-272ff33ce314')
  }
}

resource consoleRecord 'Microsoft.Network/dnsZones/A@2023-07-01-preview' = {
  parent: dnsZone
  name: 'console'
  properties: {
    TTL: 300
    ARecords: [
      {
        ipv4Address: ingressPublicIp
      }
    ]
  }
  dependsOn: [deploymentDnsZoneContributor]
}

resource wildcardPreviewRecord 'Microsoft.Network/dnsZones/A@2023-07-01-preview' = {
  parent: dnsZone
  name: '*.preview'
  properties: {
    TTL: 300
    ARecords: [
      {
        ipv4Address: ingressPublicIp
      }
    ]
  }
  dependsOn: [deploymentDnsZoneContributor]
}
