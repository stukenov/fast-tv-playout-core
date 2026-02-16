param(
  [string]$Tag = "local"
)

$ErrorActionPreference = "Stop"

Write-Host "Building service images..."
docker build -t ftvx-control-api:$Tag -f services/control-api/Dockerfile .
docker build -t ftvx-epg:$Tag -f services/epg/Dockerfile .
docker build -t ftvx-ads:$Tag -f services/ads/Dockerfile .
docker build -t ftvx-scheduler:$Tag -f services/scheduler/Dockerfile .
docker build -t ftvx-playout:$Tag -f services/playout/Dockerfile .
docker build -t ftvx-packager:$Tag -f services/packager/Dockerfile .
docker build -t ftvx-tester:$Tag -f tools/tester/Dockerfile tools/tester

Write-Host "Starting test stack..."
docker-compose -f docker-compose.test.yml up -d postgres redis origin
Start-Sleep -Seconds 3
docker-compose -f docker-compose.test.yml up -d control-api epg ads scheduler playout packager
Start-Sleep -Seconds 5
Write-Host "Run tester..."
docker-compose -f docker-compose.test.yml up --abort-on-container-exit --exit-code-from tester tester

Write-Host "Cleaning up..."
docker-compose -f docker-compose.test.yml down -v