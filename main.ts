// main.ts — entrypoint for the github-core-clone-01 v6 forge.
// Spawns the static Go core (./forge), registers its port with the
// environment, then long-polls /done until the run finishes.
import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const dir = dirname(fileURLToPath(import.meta.url));
const bin = join(dir, "forge");

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function main(): Promise<void> {
  const envUrl = process.env.MAKEFUN_ENV_URL;
  const child: ChildProcess = spawn(bin, [], {
    stdio: ["ignore", "pipe", "inherit"],
    env: process.env,
  });
  let port: number | null = null;
  let exited = false;
  let exitCode: number | null = null;
  child.on("exit", (code) => {
    exited = true;
    exitCode = code;
  });
  child.on("error", (err) => {
    console.error("failed to spawn forge:", err);
    process.exit(1);
  });

  // Read stdout until the core prints its port.
  let buf = "";
  const gotPort = new Promise<void>((resolve, reject) => {
    const to = setTimeout(() => reject(new Error("forge did not print a port")), 10000);
    child.stdout!.on("data", (chunk: Buffer) => {
      buf += chunk.toString();
      const m = buf.match(/FORGE_PORT (\d+)/);
      if (m) {
        port = parseInt(m[1], 10);
        clearTimeout(to);
        resolve();
      }
    });
    child.on("exit", () => {
      clearTimeout(to);
      reject(new Error("forge exited before printing a port"));
    });
  });
  await gotPort;

  if (!envUrl) {
    // Local run: no environment to register with; just serve.
    console.log(`forge listening on port ${port}`);
    await new Promise<void>((resolve) => child.on("exit", () => resolve()));
    process.exit(exitCode ?? 0);
  }

  // Register.
  const reg = await fetch(`${envUrl}/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ port }),
  });
  if (!reg.ok) {
    console.error("register failed:", reg.status, await reg.text());
    child.kill("SIGTERM");
    process.exit(1);
  }

  // Long-poll /done until the environment says the run is over.
  for (;;) {
    if (exited) {
      process.exit(exitCode ?? 1);
    }
    let done = false;
    try {
      const res = await fetch(`${envUrl}/done`);
      if (res.ok) {
        const body = (await res.json()) as { done?: boolean };
        done = body.done === true;
      }
    } catch {
      // Transient network error: keep polling.
    }
    if (done) break;
    await sleep(500);
  }
  child.kill("SIGTERM");
  await sleep(200);
  if (!exited) child.kill("SIGKILL");
  process.exit(0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
