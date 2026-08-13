# shellcheck shell=bash
# Sourced by local run/setup scripts. Do not execute.
#
# bash `source .env` treats STORE_RESET_CRON=0 0 * * * as STORE_RESET_CRON=0
# then runs command `0`. This loader keeps the rest of the line as the value.

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "source scripts/load-dotenv.sh; do not execute it" >&2
  exit 1
fi

load_dotenv() {
  local file="${1:-}"
  local line trimmed key val first last
  if [[ -z "$file" || ! -f "$file" ]]; then
    echo "missing dotenv file" >&2
    return 1
  fi
  set -a
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    trimmed="${line#"${line%%[![:space:]]*}"}"
    [[ -z "$trimmed" || "${trimmed:0:1}" == "#" ]] && continue
    if [[ "$trimmed" == export\ * ]]; then
      trimmed="${trimmed#export }"
      trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
    fi
    [[ "$trimmed" == *=* ]] || continue
    key="${trimmed%%=*}"
    val="${trimmed#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    if [[ ${#val} -ge 2 ]]; then
      first="${val:0:1}"
      last="${val:$((${#val} - 1)):1}"
      if [[ "$first" == "$last" && ( "$first" == '"' || "$first" == "'" ) ]]; then
        val="${val:1:$((${#val} - 2))}"
      fi
    fi
    printf -v "$key" '%s' "$val"
  done < "$file"
  set +a
}
