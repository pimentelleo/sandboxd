param location string
param controlPlaneIdentityName string
param certManagerIdentityName string
param oidcIssuerUrl string

resource controlPlaneIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: controlPlaneIdentityName
  location: location
}

resource certManagerIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: certManagerIdentityName
  location: location
}

resource controlPlaneFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: controlPlaneIdentity
  name: 'sandboxd-control-plane'
  properties: {
    audiences: ['api://AzureADTokenExchange']
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:sandboxd-system:sandboxd-control-plane'
  }
}

resource certManagerFederation 'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials@2023-01-31' = {
  parent: certManagerIdentity
  name: 'cert-manager'
  properties: {
    audiences: ['api://AzureADTokenExchange']
    issuer: oidcIssuerUrl
    subject: 'system:serviceaccount:cert-manager:cert-manager'
  }
}

output controlPlaneClientId string = controlPlaneIdentity.properties.clientId
output controlPlanePrincipalId string = controlPlaneIdentity.properties.principalId
output certManagerClientId string = certManagerIdentity.properties.clientId
output certManagerPrincipalId string = certManagerIdentity.properties.principalId
