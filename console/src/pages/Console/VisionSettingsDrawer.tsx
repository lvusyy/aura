import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Button,
  Descriptions,
  Drawer,
  Form,
  Input,
  InputNumber,
  Radio,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
  message,
} from "antd";
import { ConnectError } from "@connectrpc/connect";
import { consoleClient } from "../../api/transport";
import type { TestVisionSettingsResponse, VisionSettings } from "../../gen/aura/v1/console_pb";

export interface VisionSettingsDrawerProps {
  open: boolean;
  onClose: () => void;
  onSaved?: (settings: VisionSettings) => void;
}

interface FormValues {
  mode: string;
  localEndpoint: string;
  cloudEndpoint: string;
  model: string;
  captionMode: string;
  noulThreshold: number;
  enableBox: boolean;
  apiKeyAction: "keep" | "set" | "clear";
  newApiKey: string;
}

function describeErr(err: unknown): string {
  if (err instanceof ConnectError) {
    return err.message;
  }
  return err instanceof Error ? err.message : String(err);
}

export function VisionSettingsDrawer({ open, onClose, onSaved }: VisionSettingsDrawerProps) {
  const [form] = Form.useForm<FormValues>();
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [loaded, setLoaded] = useState<VisionSettings | null>(null);
  const [testResult, setTestResult] = useState<TestVisionSettingsResponse | null>(null);
  const [msgApi, msgCtx] = message.useMessage();

  const modeValue = Form.useWatch("mode", form) ?? "auto";
  const apiKeyActionValue = Form.useWatch("apiKeyAction", form) ?? "keep";

  const loadSettings = useCallback(async () => {
    setLoading(true);
    setTestResult(null);
    try {
      const resp = await consoleClient.getVisionSettings({});
      const s = resp.settings;
      if (s) {
        setLoaded(s);
        form.setFieldsValue({
          mode: s.mode || "auto",
          localEndpoint: s.localEndpoint || "http://192.168.22.225:30808",
          cloudEndpoint: s.cloudEndpoint || "https://api.typesafe.ai/v1/systemone",
          model: s.model || "jev-latest",
          captionMode: s.captionMode || "none",
          noulThreshold: s.noulThreshold > 0 ? s.noulThreshold : 0.5,
          enableBox: s.enableBox,
          apiKeyAction: "keep",
          newApiKey: "",
        });
      }
    } catch (err) {
      msgApi.error(`拉取视觉配置失败：${describeErr(err)}`);
    } finally {
      setLoading(false);
    }
  }, [form, msgApi]);

  useEffect(() => {
    if (open) {
      void loadSettings();
    }
  }, [open, loadSettings]);

  const handleTest = useCallback(async () => {
    const v = form.getFieldsValue();
    setTesting(true);
    setTestResult(null);
    try {
      const resp = await consoleClient.testVisionSettings({
        settings: {
          mode: v.mode,
          localEndpoint: v.localEndpoint?.trim() ?? "",
          cloudEndpoint: v.cloudEndpoint?.trim() ?? "",
          model: v.model?.trim() ?? "jev",
          captionMode: v.captionMode ?? "none",
          noulThreshold: v.noulThreshold ?? 0.5,
          enableBox: Boolean(v.enableBox),
          hasApiKey: loaded?.hasApiKey ?? false,
          apiKeyHint: loaded?.apiKeyHint ?? "",
          revision: loaded?.revision ?? 0n,
          updatedMs: loaded?.updatedMs ?? 0n,
        },
        apiKeyAction: v.apiKeyAction ?? "keep",
        newApiKey: v.newApiKey?.trim() ?? "",
      });
      setTestResult(resp);
      if (resp.ok) {
        msgApi.success(`连通正常（${Number(resp.latencyMs)} ms · ${resp.device || resp.mode}）`);
      } else {
        msgApi.warning(`探测未通过：${resp.message}`);
      }
    } catch (err) {
      msgApi.error(`探测异常：${describeErr(err)}`);
    } finally {
      setTesting(false);
    }
  }, [form, loaded, msgApi]);

  const handleSave = useCallback(async () => {
    try {
      const v = await form.validateFields();
      setSaving(true);
      const resp = await consoleClient.updateVisionSettings({
        settings: {
          mode: v.mode,
          localEndpoint: v.localEndpoint?.trim() ?? "",
          cloudEndpoint: v.cloudEndpoint?.trim() ?? "",
          model: v.model?.trim() ?? "jev",
          captionMode: v.captionMode ?? "none",
          noulThreshold: v.noulThreshold ?? 0.5,
          enableBox: Boolean(v.enableBox),
          hasApiKey: loaded?.hasApiKey ?? false,
          apiKeyHint: loaded?.apiKeyHint ?? "",
          revision: loaded?.revision ?? 0n,
          updatedMs: loaded?.updatedMs ?? 0n,
        },
        apiKeyAction: v.apiKeyAction ?? "keep",
        newApiKey: v.newApiKey?.trim() ?? "",
      });
      if (resp.settings) {
        setLoaded(resp.settings);
        form.setFieldsValue({ apiKeyAction: "keep", newApiKey: "" });
        onSaved?.(resp.settings);
      }
      msgApi.success("Jev 视觉配置已保存并即时生效");
    } catch (err) {
      msgApi.error(`保存视觉配置失败：${describeErr(err)}`);
    } finally {
      setSaving(false);
    }
  }, [form, loaded, msgApi, onSaved]);

  return (
    <Drawer
      title="Jev 视觉与快速元素选择配置"
      width={560}
      open={open}
      onClose={onClose}
      extra={
        <Space>
          <Button onClick={handleTest} loading={testing} disabled={loading}>
            测试连接
          </Button>
          <Button type="primary" onClick={handleSave} loading={saving} disabled={loading}>
            保存配置
          </Button>
        </Space>
      }
    >
      {msgCtx}
      <Space direction="vertical" size="middle" style={{ width: "100%" }}>
        <Alert
          type="info"
          showIcon
          message="三级自适应定位流水线（Tier 0 A11y → Tier 1 本地几何/k3s GPU → Tier 2 Jev 单 Token 选择）"
          description="优先通过原生无障碍树（A11y）零截图定位；未命中时按下方所选模式调用本地 k3s GPU（YOLO + Florence-2 /v1/systemone）或云端 Jev API（noul / choice / box 单 Token 概率解码）。"
        />

        <Form
          form={form}
          layout="vertical"
          initialValues={{
            mode: "auto",
            localEndpoint: "http://192.168.22.225:30808",
            cloudEndpoint: "https://api.typesafe.ai/v1/systemone",
            model: "jev-latest",
            captionMode: "none",
            noulThreshold: 0.5,
            enableBox: true,
            apiKeyAction: "keep",
            newApiKey: "",
          }}
        >
          <Form.Item label="视觉推理模式 (mode)" name="mode" required>
            <Radio.Group optionType="button" buttonStyle="solid">
              <Radio.Button value="auto">本地优先 · 云端回退 (auto)</Radio.Button>
              <Radio.Button value="local">仅本地 k3s GPU (local)</Radio.Button>
              <Radio.Button value="cloud">仅云端 Jev API (cloud)</Radio.Button>
              <Radio.Button value="disabled">仅原生 A11y (disabled)</Radio.Button>
            </Radio.Group>
          </Form.Item>

          {modeValue !== "disabled" ? (
            <>
              <Form.Item
                label="本地 k3s GPU 端点 (local_endpoint)"
                name="localEndpoint"
                extra="用于 YOLO 候选框快速几何提取及本地/本地优先模式下的 /v1/systemone 推理（默认 k3s GPU NodePort：http://192.168.22.225:30808）"
              >
                <Input placeholder="http://192.168.22.225:30808" />
              </Form.Item>

              {modeValue === "cloud" || modeValue === "auto" ? (
                <>
                  <Form.Item
                    label="云端 Jev SystemOne 端点 (cloud_endpoint)"
                    name="cloudEndpoint"
                    extra="支持 TypeSafe SystemOne (https://api.typesafe.ai/v1/systemone) 或兼容网关"
                  >
                    <Input placeholder="https://api.typesafe.ai/v1/systemone" />
                  </Form.Item>

                  <Form.Item label="云端 API 访问令牌治理" name="apiKeyAction">
                    <Radio.Group>
                      <Radio value="keep">
                        保持现有令牌{" "}
                        {loaded?.hasApiKey ? (
                          <Tag color="green">已配置 ({loaded.apiKeyHint})</Tag>
                        ) : (
                          <Tag>未配置</Tag>
                        )}
                      </Radio>
                      <Radio value="set">设置新令牌</Radio>
                      <Radio value="clear">清空令牌</Radio>
                    </Radio.Group>
                  </Form.Item>

                  {apiKeyActionValue === "set" ? (
                    <Form.Item
                      label="新云端 API 令牌 (Bearer Token)"
                      name="newApiKey"
                      rules={[{ required: true, message: "请输入新的云端 API 访问令牌" }]}
                    >
                      <Input.Password placeholder="输入 TypeSafe / OpenRouter API 令牌（保存后仅回显脱敏前缀）" />
                    </Form.Item>
                  ) : null}
                </>
              ) : null}

              <Form.Item label="Jev 模型档位 (model)" name="model">
                <Select
                  options={[
                    { label: "jev（默认极速档 · 推荐）", value: "jev" },
                    { label: "jev-large（高精度档）", value: "jev-large" },
                    { label: "jev-local-florence2-yolo（k3s 本地多模态底座）", value: "jev-local-florence2-yolo" },
                  ]}
                />
              </Form.Item>

              <Form.Item
                label="候选框描述策略 (caption_mode)"
                name="captionMode"
                extra="none（默认）跳过逐框 Florence 自回归生成，延迟从 ~1800ms 降至 ~40ms；florence 为每个图标框生成文本描述。"
              >
                <Select
                  options={[
                    { label: "none — 极速几何模式（跳过逐框自回归 Caption，推荐）", value: "none" },
                    { label: "florence — 完整逐框图标描述（耗时较高）", value: "florence" },
                  ]}
                />
              </Form.Item>

              <Space size="large" align="start">
                <Form.Item
                  label="Noul 存在性阈值"
                  name="noulThreshold"
                  extra="低于该概率判定目标不存在并快速短路"
                >
                  <InputNumber min={0.05} max={0.95} step={0.05} style={{ width: 140 }} />
                </Form.Item>

                <Form.Item
                  label="启用 Box 直接框选兜底"
                  name="enableBox"
                  valuePropName="checked"
                  extra="候选框均未命中时直接回归 [x,y,w,h]"
                >
                  <Switch />
                </Form.Item>
              </Space>
            </>
          ) : null}
        </Form>

        {testResult ? (
          <Descriptions
            bordered
            size="small"
            column={1}
            title={
              <Space>
                <span>连通性探测结果</span>
                {testResult.ok ? <Tag color="success">连通正常</Tag> : <Tag color="error">探测失败</Tag>}
              </Space>
            }
          >
            <Descriptions.Item label="模式 / 端点">
              <Typography.Text code>{testResult.mode}</Typography.Text>{" "}
              {testResult.endpoint ? <Typography.Text type="secondary">({testResult.endpoint})</Typography.Text> : null}
            </Descriptions.Item>
            <Descriptions.Item label="推理设备 / 模型">
              <Tag color="blue">{testResult.device || "-"}</Tag>
              <Typography.Text code>{testResult.model || "-"}</Typography.Text>
            </Descriptions.Item>
            <Descriptions.Item label="支持原语 (Capabilities)">
              {testResult.capabilities.length > 0 ? (
                <Space wrap size={4}>
                  {testResult.capabilities.map((cap) => (
                    <Tag key={cap} color="cyan">
                      {cap}
                    </Tag>
                  ))}
                </Space>
              ) : (
                "-"
              )}
            </Descriptions.Item>
            <Descriptions.Item label="往返耗时">
              <Typography.Text strong>{Number(testResult.latencyMs)} ms</Typography.Text>
            </Descriptions.Item>
            <Descriptions.Item label="诊断信息">{testResult.message || "-"}</Descriptions.Item>
          </Descriptions>
        ) : null}
      </Space>
    </Drawer>
  );
}
