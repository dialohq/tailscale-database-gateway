{
  pkgs,
  images,
}:
pkgs.writeShellApplication {
  name = "push-images";
  runtimeInputs = [pkgs.skopeo pkgs.crane];
  text = ''
    printf '%s' "$GITHUB_TOKEN" | skopeo login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
    printf '%s' "$GITHUB_TOKEN" | crane auth login ghcr.io --username "$GITHUB_ACTOR" --password-stdin

    ${pkgs.lib.concatStringsSep "\n" (pkgs.lib.mapAttrsToList (name: image: ''
        echo "Pushing ${image.imageRefUnsafe}..."
        if ! crane digest "${image.imageRefUnsafe}" >/dev/null 2>&1; then
          ${image.copyToRegistry}/bin/copy-to-registry --preserve-digests --dest-precompute-digests
        fi
        if [[ -n "''${PR_HEAD_COMMIT:-}" ]]; then
          crane tag "${image.imageRefUnsafe}" "pr-$PR_NUMBER-sha-$PR_HEAD_COMMIT"
        else
          crane tag "${image.imageRefUnsafe}" "sha-$GITHUB_SHA"
        fi
        echo "Completed ${name}"
      '')
      images)}
  '';
}
