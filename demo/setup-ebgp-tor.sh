#!/bin/bash
# Setup eBGP peering between cluster nodes and the TOR node for KubeVirt
# live migration e2e tests.
#
# This script configures:
#   1. A BGPPeer resource so Calico nodes peer with the TOR (ASN 63000)
#   2. A BGPFilter to export /32 host routes to the TOR
#   3. TOR BIRD kernel protocol to install BGP routes in the kernel
#
# Prerequisites:
#   - KUBECONFIG set and pointing to the cluster
#   - TOR node running BIRD via Docker (setup by gkm run setup-tor)
#   - L2TP tunnels configured (setup by gkm run setup-l2tp)
#   - TOR_IP, TOR_KEY, TOR_USER env vars set
#
# Usage:
#   export TOR_IP=<tor-external-ip>
#   export TOR_KEY=<path-to-ssh-key>
#   export TOR_USER=ubuntu  # optional, defaults to ubuntu
#   ./setup-ebgp-tor.sh

set -e

TOR_USER="${TOR_USER:-ubuntu}"

if [ -z "$TOR_IP" ] || [ -z "$TOR_KEY" ]; then
    echo "ERROR: TOR_IP and TOR_KEY must be set"
    echo "  export TOR_IP=<tor-external-ip>"
    echo "  export TOR_KEY=<path-to-ssh-key>"
    exit 1
fi

SSH_CMD="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $TOR_KEY $TOR_USER@$TOR_IP"

echo "=== Step 1: Create BGPFilter to export /32 host routes ==="
cat <<'EOF' | kubectl apply -f -
apiVersion: projectcalico.org/v3
kind: BGPFilter
metadata:
  name: export-host-routes
spec:
  exportV4:
    - action: Accept
      cidr: 192.168.0.0/16
      matchOperator: In
EOF
echo ""

echo "=== Step 2: Create BGPPeer for TOR (ASN 63000) ==="
cat <<'EOF' | kubectl apply -f -
apiVersion: projectcalico.org/v3
kind: BGPPeer
metadata:
  name: tor-peer
spec:
  peerIP: 172.16.8.5
  asNumber: 63000
  nodeSelector: all()
  filters:
    - export-host-routes
EOF
echo ""

echo "=== Step 3: Configure TOR BIRD to export routes to kernel ==="
$SSH_CMD "docker exec bird sed -i 's/export none;/export all;/' /usr/local/etc/bird.conf && docker exec bird birdcl configure" 2>&1
echo ""

echo "=== Verifying ==="
sleep 10

echo "--- Calico BIRD sessions (look for Node_172_16_8_5 Established) ---"
CALICO_POD=$(kubectl get pods -n calico-system -l k8s-app=calico-node -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n calico-system "$CALICO_POD" -c calico-node -- birdcl show protocols | grep -E "Node_172_16_8_5|Mesh_"
echo ""

echo "--- TOR BIRD sessions ---"
$SSH_CMD "docker exec bird birdcl show protocols" 2>&1 | grep -E "BGP|Established"
echo ""

echo "--- TOR /32 routes for VMs ---"
$SSH_CMD "docker exec bird birdcl show route" 2>&1 | grep "/32" | head -5
echo ""

echo "--- TOR kernel routes ---"
$SSH_CMD "ip route show proto bird" 2>&1 | head -5
echo ""

echo "=== Done ==="
echo "To run the eBGP e2e test:"
echo "  export TOR_IP=$TOR_IP"
echo "  export TOR_KEY=$TOR_KEY"
echo "  export TOR_USER=$TOR_USER"
echo "  ./e2e.test --ginkgo.label-filter='Feature:KubeVirt' --ginkgo.focus='eBGP'"
