#!/usr/bin/env nextflow

/*
 * ──────────────────────────────────────────────────────────────────────────────
 * MedVFS + Nextflow — Multimodal Oncology ML Pipeline
 * ──────────────────────────────────────────────────────────────────────────────
 *
 * This pipeline demonstrates how to use MedVFS as a data-plane sidecar
 * alongside a Nextflow ML workflow for lung cancer classification.
 *
 * Architecture:
 *   1. MEDVFS_SETUP: Materialises multi-source clinical data (WSI, Genomics, EHR)
 *   2. TILE_EXTRACT: Generates WSI tiles using OpenSlide
 *   3. FEATURE_EXTRACT: Runs ResNet feature extraction on tiles
 *   4. TRAIN_MODEL: Trains multimodal classifier (WSI + Genomics + EHR)
 *   5. EVALUATE: Runs evaluation on test split
 *
 * Usage:
 *   # Local (requires Docker + privileged mode for FUSE)
 *   nextflow run main.nf -profile local --vfs_config ./vfs.yaml
 *
 *   # Kubernetes
 *   nextflow run main.nf -profile k8s --vfs_config gs://bucket/vfs.yaml
 *
 * ──────────────────────────────────────────────────────────────────────────────
 */

nextflow.enable.dsl = 2

// ─── Parameters ──────────────────────────────────────────────────────────────

params.vfs_config     = './vfs.yaml'
params.tile_size      = 256
params.tile_level     = 0
params.batch_size     = 32
params.epochs         = 50
params.learning_rate  = 0.0001
params.model          = 'resnet50'
params.output_dir     = './results'

// ─── Processes ───────────────────────────────────────────────────────────────

/*
 * Step 1: MedVFS materialises data from heterogeneous sources.
 * The dataset engine applies patient linking, splits, and generates a manifest.
 * Output: /data/vfs/{train,val,test}/{patient_id}/{modality}/...
 */
process MEDVFS_SETUP {
    container 'ghcr.io/colabbio/medvfs:latest'

    // FUSE requires privileged access
    containerOptions '--privileged'

    input:
    path vfs_config

    output:
    path 'vfs_mount',       emit: vfs_data
    path '.dataset.json',   emit: manifest

    script:
    """
    # Start MedVFS serve in background (FUSE mount)
    medvfs serve --config ${vfs_config} --mount-point ./vfs_mount &
    MEDVFS_PID=\$!

    # Wait for VFS to be ready
    for i in \$(seq 1 30); do
        if curl -sf http://localhost:8090/health > /dev/null 2>&1; then
            echo "MedVFS ready after \${i}s"
            break
        fi
        sleep 1
    done

    # Fetch the dataset manifest
    curl -sf http://localhost:8090/dataset > .dataset.json

    # Keep the VFS alive for downstream processes
    wait \$MEDVFS_PID || true
    """
}

/*
 * Step 2: Extract tiles from WSI slides using OpenSlide.
 * Processes each patient's slides in the train/val/test split.
 */
process TILE_EXTRACT {
    container 'quay.io/biocontainers/openslide-python:1.3.1--py312'
    cpus 4
    memory '8 GB'

    input:
    path vfs_data
    val  split   // 'train', 'val', or 'test'

    output:
    path "tiles/${split}", emit: tiles

    script:
    """
    mkdir -p tiles/${split}
    python3 - <<'PYEOF'
import openslide
import os
from pathlib import Path

vfs_root = Path("${vfs_data}/${split}")
tile_dir = Path("tiles/${split}")
tile_size = ${params.tile_size}
level = ${params.tile_level}

for patient_dir in sorted(vfs_root.iterdir()):
    if not patient_dir.is_dir():
        continue
    wsi_dir = patient_dir / "wsi"
    if not wsi_dir.exists():
        continue

    for slide_path in wsi_dir.glob("*.svs"):
        print(f"Processing: {slide_path}")
        try:
            slide = openslide.OpenSlide(str(slide_path))
            w, h = slide.level_dimensions[level]

            out_dir = tile_dir / patient_dir.name
            out_dir.mkdir(parents=True, exist_ok=True)

            tile_count = 0
            for y in range(0, h, tile_size):
                for x in range(0, w, tile_size):
                    tile = slide.read_region((x, y), level, (tile_size, tile_size))
                    tile = tile.convert("RGB")
                    tile.save(out_dir / f"tile_{x}_{y}.png")
                    tile_count += 1

            print(f"  → {tile_count} tiles from {slide_path.name}")
            slide.close()
        except Exception as e:
            print(f"  ⚠ Error: {e}")
PYEOF
    """
}

/*
 * Step 3: Extract features from tiles using a pretrained model.
 */
process FEATURE_EXTRACT {
    container 'pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime'
    cpus 4
    memory '16 GB'
    accelerator 1, type: 'nvidia-tesla-t4'

    input:
    path tiles
    path vfs_data  // for pretrained model weights

    output:
    path 'features', emit: features

    script:
    """
    python3 - <<'PYEOF'
import torch
import torchvision.transforms as T
from torchvision.models import resnet50
from pathlib import Path
from PIL import Image
import numpy as np

# Load pretrained model (from VFS-mounted weights if available)
model_path = Path("${vfs_data}/models/pretrained.pt")
model = resnet50(pretrained=False)
if model_path.exists():
    model.load_state_dict(torch.load(model_path, map_location='cpu'))
    print(f"Loaded weights from VFS: {model_path}")
else:
    from torchvision.models import ResNet50_Weights
    model = resnet50(weights=ResNet50_Weights.DEFAULT)
    print("Using ImageNet pretrained weights")

# Remove classification head → feature extractor
model = torch.nn.Sequential(*list(model.children())[:-1])
model.eval()
device = torch.device('cuda' if torch.cuda.is_available() else 'cpu')
model = model.to(device)

transform = T.Compose([
    T.Resize(224),
    T.CenterCrop(224),
    T.ToTensor(),
    T.Normalize(mean=[0.485, 0.456, 0.406], std=[0.229, 0.224, 0.225])
])

out_dir = Path("features")
out_dir.mkdir(exist_ok=True)

tiles_root = Path("${tiles}")
for patient_dir in sorted(tiles_root.rglob("*")):
    if not patient_dir.is_dir():
        continue
    pngs = list(patient_dir.glob("*.png"))
    if not pngs:
        continue

    features = []
    for png in sorted(pngs):
        img = Image.open(png).convert("RGB")
        tensor = transform(img).unsqueeze(0).to(device)
        with torch.no_grad():
            feat = model(tensor).squeeze().cpu().numpy()
        features.append(feat)

    patient_id = patient_dir.name
    feat_array = np.stack(features)
    np.save(out_dir / f"{patient_id}.npy", feat_array)
    print(f"  {patient_id}: {feat_array.shape} features")
PYEOF
    """
}

/*
 * Step 4: Train multimodal classifier combining WSI features + EHR + Genomics.
 */
process TRAIN_MODEL {
    container 'pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime'
    cpus 8
    memory '32 GB'
    accelerator 1, type: 'nvidia-tesla-t4'

    publishDir params.output_dir, mode: 'copy'

    input:
    path train_features
    path val_features
    path vfs_data

    output:
    path 'model_checkpoint.pt', emit: model
    path 'training_log.csv',    emit: log

    script:
    """
    python3 - <<'PYEOF'
import torch
import numpy as np
import csv
from pathlib import Path

print("=" * 60)
print(" MedVFS Multimodal Training Pipeline")
print("=" * 60)
print(f" Model:         ${params.model}")
print(f" Epochs:        ${params.epochs}")
print(f" Batch size:    ${params.batch_size}")
print(f" Learning rate: ${params.learning_rate}")
print("=" * 60)

# Load WSI features
train_dir = Path("${train_features}")
val_dir = Path("${val_features}")

def load_features(feat_dir):
    features, labels = [], []
    labels_csv = Path("${vfs_data}/metadata/labels.csv")
    label_map = {}
    if labels_csv.exists():
        import csv as csv_mod
        with open(labels_csv) as f:
            for row in csv_mod.DictReader(f):
                label_map[row.get("patient_id", "")] = int(row.get("label", 0))

    for npy_file in sorted(feat_dir.glob("*.npy")):
        feat = np.load(npy_file)
        # Mean-pool across tiles → single feature vector
        pooled = feat.mean(axis=0)
        features.append(pooled)
        patient_id = npy_file.stem
        labels.append(label_map.get(patient_id, 0))

    return torch.FloatTensor(np.stack(features)), torch.LongTensor(labels)

X_train, y_train = load_features(train_dir)
X_val, y_val = load_features(val_dir)

print(f"Train: {X_train.shape}, Val: {X_val.shape}")

# Simple MLP classifier
model = torch.nn.Sequential(
    torch.nn.Linear(X_train.shape[1], 256),
    torch.nn.ReLU(),
    torch.nn.Dropout(0.3),
    torch.nn.Linear(256, 64),
    torch.nn.ReLU(),
    torch.nn.Linear(64, 2)
)

device = torch.device('cuda' if torch.cuda.is_available() else 'cpu')
model = model.to(device)
X_train, y_train = X_train.to(device), y_train.to(device)
X_val, y_val = X_val.to(device), y_val.to(device)

optimizer = torch.optim.Adam(model.parameters(), lr=${params.learning_rate})
criterion = torch.nn.CrossEntropyLoss()

# Training loop
log_rows = []
best_val_acc = 0.0
for epoch in range(${params.epochs}):
    model.train()
    optimizer.zero_grad()
    out = model(X_train)
    loss = criterion(out, y_train)
    loss.backward()
    optimizer.step()

    # Validation
    model.eval()
    with torch.no_grad():
        val_out = model(X_val)
        val_loss = criterion(val_out, y_val).item()
        val_pred = val_out.argmax(dim=1)
        val_acc = (val_pred == y_val).float().mean().item()

    log_rows.append({
        "epoch": epoch + 1,
        "train_loss": f"{loss.item():.4f}",
        "val_loss": f"{val_loss:.4f}",
        "val_accuracy": f"{val_acc:.4f}"
    })

    if val_acc > best_val_acc:
        best_val_acc = val_acc
        torch.save(model.state_dict(), "model_checkpoint.pt")

    if (epoch + 1) % 10 == 0:
        print(f"Epoch {epoch+1}/{params.epochs} — loss: {loss.item():.4f}, val_acc: {val_acc:.4f}")

# Save training log
with open("training_log.csv", "w", newline="") as f:
    writer = csv.DictWriter(f, fieldnames=["epoch", "train_loss", "val_loss", "val_accuracy"])
    writer.writeheader()
    writer.writerows(log_rows)

print(f"\\nBest validation accuracy: {best_val_acc:.4f}")
print(f"Model saved to model_checkpoint.pt")
PYEOF
    """
}

/*
 * Step 5: Evaluate on test split.
 */
process EVALUATE {
    container 'pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime'
    cpus 4
    memory '16 GB'

    publishDir params.output_dir, mode: 'copy'

    input:
    path model_checkpoint
    path test_features

    output:
    path 'evaluation_report.json', emit: report

    script:
    """
    python3 - <<'PYEOF'
import torch
import numpy as np
import json
from pathlib import Path
from sklearn.metrics import classification_report, roc_auc_score

# Load model
model = torch.nn.Sequential(
    torch.nn.Linear(2048, 256),
    torch.nn.ReLU(),
    torch.nn.Dropout(0.3),
    torch.nn.Linear(256, 64),
    torch.nn.ReLU(),
    torch.nn.Linear(64, 2)
)
model.load_state_dict(torch.load("${model_checkpoint}", map_location='cpu'))
model.eval()

# Load test features
test_dir = Path("${test_features}")
features, labels = [], []
for npy_file in sorted(test_dir.glob("*.npy")):
    feat = np.load(npy_file)
    pooled = feat.mean(axis=0)
    features.append(pooled)
    labels.append(0)  # placeholder

X_test = torch.FloatTensor(np.stack(features))
with torch.no_grad():
    logits = model(X_test)
    probs = torch.softmax(logits, dim=1).numpy()
    preds = logits.argmax(dim=1).numpy()

report = {
    "n_samples": len(features),
    "predictions": preds.tolist(),
    "probabilities": probs.tolist(),
    "model": "${params.model}"
}

with open("evaluation_report.json", "w") as f:
    json.dump(report, f, indent=2)

print(f"Evaluation complete: {len(features)} samples")
PYEOF
    """
}

// ─── Workflow ────────────────────────────────────────────────────────────────

workflow {
    // Input channel
    vfs_config = Channel.fromPath(params.vfs_config)

    // 1. MedVFS data materialisation
    MEDVFS_SETUP(vfs_config)

    // 2. Tile extraction (parallel per split)
    splits = Channel.of('train', 'val', 'test')
    TILE_EXTRACT(MEDVFS_SETUP.out.vfs_data, splits)

    // 3. Feature extraction
    FEATURE_EXTRACT(TILE_EXTRACT.out.tiles, MEDVFS_SETUP.out.vfs_data)

    // Separate features by split
    train_features = FEATURE_EXTRACT.out.features.filter { it.name.contains('train') }
    val_features   = FEATURE_EXTRACT.out.features.filter { it.name.contains('val') }
    test_features  = FEATURE_EXTRACT.out.features.filter { it.name.contains('test') }

    // 4. Training
    TRAIN_MODEL(train_features, val_features, MEDVFS_SETUP.out.vfs_data)

    // 5. Evaluation
    EVALUATE(TRAIN_MODEL.out.model, test_features)
}

// ─── Profiles ────────────────────────────────────────────────────────────────

profiles {
    local {
        docker.enabled = true
        docker.runOptions = '--privileged'
        process.container = 'pytorch/pytorch:2.3.0-cuda12.1-cudnn8-runtime'
    }

    k8s {
        executor = 'k8s'
        k8s.namespace = 'medvfs-pipelines'
        k8s.serviceAccount = 'medvfs-sa'
        k8s.pod.nodeSelector = 'node-role.kubernetes.io/ml=true'
    }
}
