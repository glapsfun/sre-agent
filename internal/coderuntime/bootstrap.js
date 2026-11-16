import fs from "node:fs";
import net from "node:net";

const stringify = JSON.stringify.bind(JSON);
const parse = JSON.parse.bind(JSON);
const write = process.stdout.write.bind(process.stdout);
const exit = process.exit.bind(process);
const fail = () => {
  process.stderr.write("runtime program failed\n", () => exit(1));
};
process.on("uncaughtException", fail);
process.on("unhandledRejection", fail);

try {
  const limits = fs.readFileSync("/proc/self/limits", "utf8");
  for (const [name, value] of [["Max cpu time", "5"], ["Max core file size", "0"], ["Max file size", "2097152"]]) {
    const line = limits.split("\n").find(line => line.startsWith(name));
    const fields = line?.slice(name.length).trim().split(/\s+/);
    if (!fields || fields[0] !== value || fields[1] !== value) throw new Error();
  }
  let buffer = Buffer.alloc(0);
  let initial;
  const invocation = new Promise(resolve => { initial = resolve; });
  const pending = new Map();
  const queue = [];
  let nextID = 0;
  const emit = value => new Promise((resolve, reject) => {
    write(stringify(value) + "\n", error => error ? reject(error) : resolve());
  });
  function dispatch() {
    while (pending.size < 2 && queue.length) {
      const call = queue.shift();
      const id = String(++nextID);
      pending.set(id, call);
      emit({type: "call", id, key: call.key}).catch(fail);
    }
  }
  function input(chunk) {
    buffer = Buffer.concat([buffer, chunk]);
    const limit = initial ? 65536 : 2 * 1024 * 1024;
    while (true) {
      const index = buffer.indexOf(10);
      if (index < 0) { if (buffer.length > limit) fail(); return; }
      if (index + 1 > limit) { fail(); return; }
      const value = parse(buffer.subarray(0, index).toString("utf8"));
      buffer = buffer.subarray(index + 1);
      if (initial) { const resolve = initial; initial = null; resolve(value); }
      else {
        const call = pending.get(value.id);
        if (!call) throw new Error();
        pending.delete(value.id);
        call.resolve({ok: value.ok, result: value.result});
        dispatch();
      }
    }
  }
  process.stdin.on("data", input);
  process.stdin.on("error", fail);
  const inputEnded = new Promise(resolve => process.stdin.once("end", resolve));
  await emit({type: "ready"});
  const inv = await invocation;
  let relay;
  let proxy_url = null;
  const sockets = new Set();
  if (inv.phase === "source") {
    relay = net.createServer({allowHalfOpen: true}, client => {
      const upstream = net.createConnection({path: "/run/egress.sock", allowHalfOpen: true});
      for (const socket of [client, upstream]) {
        sockets.add(socket);
        socket.on("close", () => sockets.delete(socket));
      }
      client.on("error", () => upstream.destroy());
      upstream.on("error", () => client.destroy());
      client.pipe(upstream);
      upstream.pipe(client);
    });
    relay.maxConnections = 8;
    await new Promise((resolve, reject) => {
      relay.once("error", reject);
      relay.listen(0, "127.0.0.1", resolve);
    });
    proxy_url = `http://127.0.0.1:${relay.address().port}`;
    process.env.HTTP_PROXY = proxy_url;
    process.env.HTTPS_PROXY = proxy_url;
  }
  const collection = {...inv, proxy_url, call(key) {
    if (inv.phase !== "source" || !inv.keys.some(item => item.key === key)) return Promise.reject(new Error("unavailable helper"));
    return new Promise((resolve, reject) => { queue.push({key, resolve, reject}); dispatch(); });
  }};
  const AsyncFunction = Object.getPrototypeOf(async function(){}).constructor;
  const program = fs.readFileSync("/runtime/program.js", "utf8");
  const values = await new AsyncFunction("collection", program)(collection);
  if (pending.size || queue.length) throw new Error();
  await emit({type: "final", values});
  // EOF acknowledges host validation and proxy shutdown before relay teardown.
  await inputEnded;
  for (const socket of sockets) socket.destroy();
  if (relay) relay.close();
  process.stdin.off("data", input);
  process.stdin.pause();
  exit(0);
} catch {
  fail();
}
