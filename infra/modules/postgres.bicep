param location string
param serverName string
param delegatedSubnetId string
param vnetId string
param administratorLogin string
@secure()
param administratorPassword string
param primaryZone string
param standbyZone string
param logAnalyticsWorkspaceId string

resource privateDnsZone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: 'private.postgres.database.azure.com'
  location: 'global'
}

resource dnsLink 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: privateDnsZone
  name: 'aks-vnet'
  location: 'global'
  properties: {
    registrationEnabled: false
    virtualNetwork: {
      id: vnetId
    }
  }
}

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2024-08-01' = {
  name: serverName
  location: location
  sku: {
    name: 'Standard_D4ds_v5'
    tier: 'GeneralPurpose'
  }
  properties: {
    version: '16'
    administratorLogin: administratorLogin
    administratorLoginPassword: administratorPassword
    availabilityZone: primaryZone
    highAvailability: {
      mode: 'ZoneRedundant'
      standbyAvailabilityZone: standbyZone
    }
    storage: {
      storageSizeGB: 128
      autoGrow: 'Enabled'
      tier: 'P10'
    }
    backup: {
      backupRetentionDays: 35
      geoRedundantBackup: 'Enabled'
    }
    network: {
      delegatedSubnetResourceId: delegatedSubnetId
      privateDnsZoneArmResourceId: privateDnsZone.id
    }
  }
}

resource sandboxdDatabase 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = {
  parent: server
  name: 'sandboxd'
  properties: {
    charset: 'UTF8'
    collation: 'en_US.utf8'
  }
}

resource diagnostics 'Microsoft.Insights/diagnosticSettings@2021-05-01-preview' = {
  scope: server
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

output fqdn string = server.properties.fullyQualifiedDomainName
