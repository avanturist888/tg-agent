# Install a worker build returned by wk get.
param([Parameter(Mandatory=$true)][string]$Artifacts, [Parameter(Mandatory=$true)][string]$BaselinePath)
& "$PSScriptRoot\install-build.ps1" -Artifacts $Artifacts -BaselinePath $BaselinePath
