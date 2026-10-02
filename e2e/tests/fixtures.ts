// Shared fixtures: the base URL and setup code of the server started by
// global-setup.ts, and the services the scripts/test-e2e-docker.sh containers expose.
import { readFileSync } from "node:fs";
import { test as base, expect } from "@playwright/test";
import { STATE_FILE, type ServerState } from "../paths.js";

/** Services are the disposable MongoDB and MinIO of scripts/test-e2e-docker.sh. */
export interface Services {
  mongoURI: string;
  database: string;
  s3Endpoint: string;
  s3Bucket: string;
  s3AccessKey: string;
  s3SecretKey: string;
}

function required(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set: run the suite with "make test-e2e"`);
  return v;
}

export const test = base.extend<{ server: ServerState; services: Services }>({
  server: async ({}, use) => {
    await use(JSON.parse(readFileSync(STATE_FILE, "utf8")) as ServerState);
  },
  baseURL: async ({ server }, use) => {
    await use(server.baseURL);
  },
  services: async ({}, use) => {
    await use({
      mongoURI: required("MONGORESCUE_E2E_MONGO_URI"),
      database: process.env.MONGORESCUE_E2E_DATABASE || "e2e_shop",
      s3Endpoint: required("MONGORESCUE_E2E_S3_ENDPOINT"),
      s3Bucket: required("MONGORESCUE_E2E_S3_BUCKET"),
      s3AccessKey: required("MONGORESCUE_E2E_S3_ACCESS_KEY"),
      s3SecretKey: required("MONGORESCUE_E2E_S3_SECRET_KEY"),
    });
  },
});

export { expect };

/** ADMIN_USERNAME is the administrator created by auth.setup.ts. */
export const ADMIN_USERNAME = "e2e-admin";

/** CONNECTION_NAME is the MongoDB connection added by 02-connection.spec.ts. */
export const CONNECTION_NAME = "e2e-mongo";

/** TARGET_NAME is the MinIO storage target added by 03-storage-target.spec.ts. */
export const TARGET_NAME = "e2e-minio";
