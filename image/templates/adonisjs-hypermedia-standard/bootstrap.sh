#!/usr/bin/env bash
# First boot creates the selected AdonisJS starter. Later boots only build when
# source files changed, preserving the generated application and SQLite state.
set -euo pipefail

app_dir="$(pwd)"
marker="$app_dir/bootstrap.sh"
build_stamp="$app_dir/build/.sandboxd-adonis-built"
starter_kit="github:adonisjs/starter-kits/hypermedia#919a6e8ac1b2f347ace03d3bc0a30465dc33bcbd"
temp_dir=""

cleanup() {
  if [ -n "$temp_dir" ] && [ -d "$temp_dir" ]; then
    rm -rf "$temp_dir"
  fi
}
trap cleanup EXIT

fail() {
  printf 'sandboxd AdonisJS bootstrap: %s\n' "$*" >&2
  exit 1
}

has_application() {
  [ -f "$app_dir/package.json" ] && [ -f "$app_dir/ace.js" ]
}

create_application() {
  if has_application; then
    rm -f "$marker"
    return
  fi
  if [ ! -f "$marker" ]; then
    fail "workspace is incomplete; restore the application instead of recreating it"
  fi

  temp_dir="$(mktemp -d)"
  npx --yes create-adonisjs@3.4.0 "$temp_dir/app" \
    "--kit=$starter_kit" \
    --pkg=pnpm
  rm -rf "$temp_dir/app/.git"
  cp -a "$temp_dir/app/." "$app_dir/"
  rm -f "$app_dir/.gitkeep" "$marker"
}

install_dependencies() {
  if [ ! -x "$app_dir/node_modules/.bin/adonis-kit" ]; then
    (
      cd "$app_dir"
      pnpm install
    )
  fi
}

build_application() {
  create_application
  install_dependencies
  [ -f "$app_dir/.env" ] || fail "generated .env is missing"

  (
    cd "$app_dir"
    pnpm run build
  )
  cp "$app_dir/.env" "$app_dir/build/.env"
  mkdir -p "$app_dir/tmp"
  rm -rf "$app_dir/build/tmp"
  ln -s ../tmp "$app_dir/build/tmp"
  (
    cd "$app_dir/build"
    NODE_ENV=production node ace migration:run --force
  )
  touch "$build_stamp"
}

needs_build() {
  if [ ! -x "$app_dir/node_modules/.bin/adonis-kit" ] ||
    [ ! -f "$app_dir/build/bin/server.js" ] ||
    [ ! -f "$build_stamp" ]; then
    return 0
  fi
  find "$app_dir" \
    \( -path "$app_dir/.git" -o -path "$app_dir/build" -o -path "$app_dir/node_modules" -o -path "$app_dir/tmp" \) -prune -o \
    -type f -newer "$build_stamp" -print -quit | grep -q .
}

case "${1:---serve}" in
  --build)
    build_application
    ;;
  --serve)
    create_application
    if needs_build; then
      build_application
    fi
    cd "$app_dir/build"
    exec env HOST=0.0.0.0 PORT=3000 NODE_ENV=production node bin/server.js
    ;;
  *)
    fail "usage: bootstrap.sh [--build|--serve]"
    ;;
esac
