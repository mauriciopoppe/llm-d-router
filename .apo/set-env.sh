# Base Runtime Environment Module
export SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"

# Ensure we can find packages installed in real user home
REAL_USER=$(whoami)
PYTHON_VERSION=$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
export PYTHONPATH="/usr/local/google/home/${REAL_USER}/.local/lib/python${PYTHON_VERSION}/site-packages:${PYTHONPATH}"

# When running inside ephemeral git worktrees, discover parent repo root and symlink .venv
GIT_COMMON_DIR=$(git rev-parse --git-common-dir 2>/dev/null || true)
if [ -n "$GIT_COMMON_DIR" ] && [ "$GIT_COMMON_DIR" != ".git" ]; then
  PARENT_REPO_ROOT=$(cd "$GIT_COMMON_DIR/.." && pwd)
  if [ -d "${PARENT_REPO_ROOT}/.apo/.venv" ] && [ ! -d "${SCRIPT_DIR}/.venv" ]; then
    ln -s "${PARENT_REPO_ROOT}/.apo/.venv" "${SCRIPT_DIR}/.venv"
  fi
fi

# Initialize and activate Python virtual environment
if [ -f "${SCRIPT_DIR}/.venv/bin/activate" ]; then
  source "${SCRIPT_DIR}/.venv/bin/activate"
else
  echo "Initializing Python virtual environment..."
  python3 -m venv "${SCRIPT_DIR}/.venv"
  touch "${SCRIPT_DIR}/.venv/DONT_FOLLOW_SYMLINKS_WHEN_TRAVERSING_THIS_DIRECTORY_VIA_A_RECURSIVE_TARGET_PATTERN"
  touch "${SCRIPT_DIR}/.venv/bin/DONT_FOLLOW_SYMLINKS_WHEN_TRAVERSING_THIS_DIRECTORY_VIA_A_RECURSIVE_TARGET_PATTERN"
  source "${SCRIPT_DIR}/.venv/bin/activate"
  python3 -m pip install optuna pyyaml --quiet -i https://pypi.org/simple
fi

# Source hardware stack parameters staged by the orchestrator in the worktree
if [ -f "${SCRIPT_DIR}/stack.env" ]; then
  source "${SCRIPT_DIR}/stack.env"
fi

# Isolate scratch directory to workspace
export SCRATCH_DIR="${SCRIPT_DIR}/scratch"
mkdir -p "${SCRATCH_DIR}"
# Local Execution Environment Module
export LOCAL_WORKLOAD=true
