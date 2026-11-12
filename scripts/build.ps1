# Install a worker build returned by wk get.
param([Parameter(Mandatory=$true)][string]$Artifacts)
& "$PSScriptRoot\install-build.ps1" -Artifacts $Artifacts
