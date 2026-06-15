/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // Proxy /api to the Go server so browser requests stay same-origin.
  async rewrites() {
    const target = process.env.API_PROXY_TARGET || "http://localhost:8080";
    return [{ source: "/api/:path*", destination: `${target}/api/:path*` }];
  },
};

export default nextConfig;
