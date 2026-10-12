{
  description = "fix2 benchmarks: the toolchains of the agents";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (
        pkgs:
        let
          # Every agent's engine runs on the same JDK, the latest LTS.
          jdk = pkgs.jdk25;
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go_1_27
              jdk
              (pkgs.maven.override { jdk_headless = jdk; })
              (pkgs.gradle_9.override { java = jdk; })
              pkgs.gnumake
            ];
            JAVA_HOME = jdk.home;
            # The Go of the shell, never one go.mod would download.
            GOTOOLCHAIN = "local";
          };
        }
      );
    };
}
