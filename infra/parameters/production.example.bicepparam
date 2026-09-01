using '../main.bicep'

// Copy this file outside source control, replace every value, and pass PostgreSQL
// credentials through a secure parameter mechanism rather than placing them here.
// The deployment command must provide POSTGRES_ADMIN_PASSWORD; this file contains
// no credential value.
param workloadPrefix = '<globally-unique-prefix>'
param location = 'brazilsouth'
param vnetAddressPrefix = '10.42.0.0/16'
param aksSubnetPrefix = '10.42.0.0/20'
param privateEndpointSubnetPrefix = '10.42.16.0/24'
param postgresSubnetPrefix = '10.42.17.0/28'
param dnsZoneResourceId = '/subscriptions/<subscription-id>/resourceGroups/<dns-resource-group>/providers/Microsoft.Network/dnsZones/<root-domain>'
param adminGroupObjectId = '<entra-admin-security-group-object-id>'
param deploymentPrincipalObjectId = readEnvironmentVariable('DEPLOYMENT_PRINCIPAL_OBJECT_ID')
param postgresAdminLogin = '<postgres-admin-login>'
param postgresAdminPassword = readEnvironmentVariable('POSTGRES_ADMIN_PASSWORD')
param kubernetesVersion = '<aks-version-available-in-brazil-south>'
param sandboxVmSize = '<kata-supported-nested-virtualization-vm-sku>'
param systemVmSize = 'Standard_D4s_v5'
param aksAdminUsername = '<break-glass-admin-username>'
param aksAdminSshPublicKey = '<ssh-ed25519-public-key>'
param postgresPrimaryZone = '1'
param postgresStandbyZone = '2'
