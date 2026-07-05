#!/usr/bin/env bash
# MedVFS Integration Test Runner
#
# Runs Docker-based integration tests validating POSIX compatibility
# of the FUSE mount with clinical processing tools.
#
# Usage:
#   ./tests/integration/run_integration.sh
#
# Requirements:
#   - Docker and Docker Compose v2
#   - Privileged container support (for FUSE)
#
# Exit codes:
#   0 = all tests passed
#   1 = one or more tests failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "╔══════════════════════════════════════════════════╗"
echo "║   MedVFS Integration Test Suite                  ║"
echo "╚══════════════════════════════════════════════════╝"
echo ""
echo "Project root: $PROJECT_ROOT"
echo "Compose file: $SCRIPT_DIR/docker-compose.test.yaml"
echo ""

# Create fixtures directory
mkdir -p "$SCRIPT_DIR/fixtures"

# Run tests
echo "▶ Starting integration tests..."
cd "$SCRIPT_DIR"

docker compose -f docker-compose.test.yaml up \
  --build \
  --abort-on-container-exit \
  --exit-code-from test-openslide \
  2>&1

EXIT_CODE=$?

echo ""
if [ $EXIT_CODE -eq 0 ]; then
  echo "✅ All integration tests PASSED"
else
  echo "❌ Integration tests FAILED (exit=$EXIT_CODE)"
  echo ""
  echo "Check individual container logs:"
  echo "  docker compose -f docker-compose.test.yaml logs test-openslide"
  echo "  docker compose -f docker-compose.test.yaml logs test-clamav"
  echo "  docker compose -f docker-compose.test.yaml logs test-libvips"
  echo "  docker compose -f docker-compose.test.yaml logs test-samtools"
fi

# Cleanup
echo ""
echo "▶ Cleaning up containers..."
docker compose -f docker-compose.test.yaml down -v --remove-orphans 2>/dev/null || true

exit $EXIT_CODE
