import { createServer, type ViteDevServer } from "vite";

export async function withSSR<T>(
  load: (vite: Pick<ViteDevServer, "ssrLoadModule">) => Promise<T>,
): Promise<T> {
  const vite = await createServer({
    root: process.cwd(),
    server: { middlewareMode: true },
    appType: "custom",
    logLevel: "silent",
    ssr: { noExternal: [/^@mui\//, /^react-transition-group/] },
  });
  try {
    return await load(vite);
  } finally {
    await vite.close();
  }
}
