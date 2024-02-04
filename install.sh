#!/usr/bin/env bash

APP_NAME="filterbox"
REPO_URL="https://github.com/PlainsightAI/filterbox"

: ${USE_SUDO:="true"}
: ${FILTERBOX_INSTALL_DIR:="/usr/local/bin"}


# initArch discovers the architecture for this system.
initArch() {
  ARCH=$(uname -m)
  case $ARCH in
    aarch64) 
        ARCH="arm64"
    ;;
    x86_64) 
        ARCH="x86_64"
    ;;
    *)
        echo "Error: Unsupported architecture ($ARCH). Only arm64 and amd64 are supported."
        exit 1
    ;;    
  esac
}


runAsRoot() {
  local CMD="$*"

  if [ $EUID -ne 0 -a $USE_SUDO = "true" ]; then
    CMD="sudo $CMD"
  fi

  $CMD
}

scurl() {
  curl --proto "=https" --tlsv1.2 --fail --show-error "$@"
}

checkTagProvided() {
  [[ ! -z "$TAG" ]]
}

checkLatestVersion() {
  local latest_release_url="$REPO_URL/releases/latest"
  if type "curl" > /dev/null; then
    TAG=$(scurl -Ls -o /dev/null -w %{url_effective} $latest_release_url | grep -oE "[^/]+$" )
  elif type "wget" > /dev/null; then
    TAG=$(wget $latest_release_url --server-response -O /dev/null 2>&1 | awk '/^\s*Location: /{DEST=$2} END{ print DEST}' | grep -oE "[^/]+$")
  fi
}

downloadFile() {
  FILTERBOX_DIST="filterbox_Linux_$ARCH.tar.gz"
  DOWNLOAD_URL="$REPO_URL/releases/download/$TAG/$FILTERBOX_DIST"
  FILTERBOX_TMP_ROOT="$(mktemp -dt filterbox-binary-XXXXXX)"
  FILTERBOX_TMP_FILE="$FILTERBOX_TMP_ROOT/$FILTERBOX_DIST"
  if type "curl" > /dev/null; then
    scurl -sL "$DOWNLOAD_URL" -o "$FILTERBOX_TMP_FILE"
  elif type "wget" > /dev/null; then
    wget -q -O "$FILTERBOX_TMP_FILE" "$DOWNLOAD_URL"
  fi
}

installFile() {
  echo "Preparing to install $APP_NAME into ${FILTERBOX_INSTALL_DIR}"
  runAsRoot mv $APP_NAME "$FILTERBOX_INSTALL_DIR/$APP_NAME"
  echo "$APP_NAME installed into $FILTERBOX_INSTALL_DIR/$APP_NAME"
}

cleanup() {
  if [[ -d "${K3D_TMP_ROOT:-}" ]]; then
    rm -rf "$K3D_TMP_ROOT"
  fi
}

fail_trap() {
  result=$?
  if [ "$result" != "0" ]; then
    if [[ -n "$INPUT_ARGUMENTS" ]]; then
      echo "Failed to install $APP_NAME with the arguments provided: $INPUT_ARGUMENTS"
      help
    else
      echo "Failed to install $APP_NAME"
    fi
    echo -e "\tFor support, go to $REPO_URL."
  fi
  cleanup
  exit $result
}

testVersion() {
  if ! command -v $APP_NAME &> /dev/null; then
    echo "$APP_NAME not found. Is $FILTERBOX_INSTALL_DIR on your "'$PATH?'
    exit 1
  fi
  echo "Run '$APP_NAME --help' to see what you can do with it."
}

help () {
  echo "Accepted cli arguments are:"
  echo -e "\t[--help|-h ] ->> prints this help"
}


extractTarBall(){
    tar -xf $FILTERBOX_TMP_FILE
}

checkInstalledVersion() {
  if [[ -f "${FILTERBOX_INSTALL_DIR}/${APP_NAME}" ]]; then
    local version=$(filterbox version | grep 'filterbox version' | cut -d " " -f3)
    if [[ "$version" == "$TAG" ]]; then
      echo "filterbox ${version} is already ${DESIRED_VERSION:-latest}"
      return 0
    else
      echo "filterbox ${TAG} is available. Changing from version ${version}."
      return 1
    fi
  else
    return 1
  fi
}


# Execution

trap "fail_trap" EXIT
set -e


export INPUT_ARGUMENTS="${@}"
set -u
while [[ $# -gt 0 ]]; do
  case $1 in
    '--help'|-h)
       help
       exit 0
       ;;
    *) exit 1
       ;;
  esac
  shift
done
set +u

initArch

checkTagProvided || checkLatestVersion
if ! checkInstalledVersion; then
  downloadFile
  extractTarBall
  installFile
fi
testVersion
cleanup