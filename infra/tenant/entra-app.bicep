targetScope = 'tenant'
extension microsoftGraphV1

@description('Display name for the sandboxd Entra application. It is intentionally supplied by the operator.')
param applicationDisplayName string
@description('Object ID of the existing Entra security group granted sandboxd.user.')
param userGroupObjectId string
@description('Object ID of the existing Entra security group granted sandboxd.admin.')
param adminGroupObjectId string
@description('Optional homepage URI for the console, such as https://console.example.com.')
param homepageUrl string

resource sandboxdApplication 'Microsoft.Graph/applications@v1.0' = {
  uniqueName: applicationDisplayName
  displayName: applicationDisplayName
  signInAudience: 'AzureADMyOrg'
  web: {
    homePageUrl: homepageUrl
    redirectUris: [
      '${homepageUrl}/auth/callback'
    ]
  }
  appRoles: [
    {
      id: guid(applicationDisplayName, 'sandboxd.user')
      allowedMemberTypes: ['User']
      description: 'Use the sandboxd console and API.'
      displayName: 'sandboxd user'
      isEnabled: true
      value: 'sandboxd.user'
    }
    {
      id: guid(applicationDisplayName, 'sandboxd.admin')
      allowedMemberTypes: ['User']
      description: 'Administer sandboxd and its production settings.'
      displayName: 'sandboxd administrator'
      isEnabled: true
      value: 'sandboxd.admin'
    }
  ]
}

resource sandboxdServicePrincipal 'Microsoft.Graph/servicePrincipals@v1.0' = {
  appId: sandboxdApplication.appId
}

resource userRoleAssignment 'Microsoft.Graph/appRoleAssignedTo@v1.0' = {
  appRoleId: guid(applicationDisplayName, 'sandboxd.user')
  principalId: userGroupObjectId
  resourceId: sandboxdServicePrincipal.id
}

resource adminRoleAssignment 'Microsoft.Graph/appRoleAssignedTo@v1.0' = {
  appRoleId: guid(applicationDisplayName, 'sandboxd.admin')
  principalId: adminGroupObjectId
  resourceId: sandboxdServicePrincipal.id
}

output applicationClientId string = sandboxdApplication.appId
output servicePrincipalObjectId string = sandboxdServicePrincipal.id
