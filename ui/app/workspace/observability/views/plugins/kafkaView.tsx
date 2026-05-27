import { getErrorMessage, useAppSelector, useUpdatePluginMutation } from "@/lib/store";
import { type KafkaConfigSchema, type KafkaFormSchema } from "@/lib/types/schemas";
import { toOptionalEnvVarPayload } from "@/lib/utils/envVarForm";
import { useMemo } from "react";
import { toast } from "sonner";
import { KafkaFormFragment } from "../../fragments/kafkaFormFragment";

interface KafkaViewProps {
	onDelete?: () => void;
	isDeleting?: boolean;
}

export default function KafkaView({ onDelete, isDeleting }: KafkaViewProps) {
	const selectedPlugin = useAppSelector((state) => state.plugin.selectedPlugin);
	const [updatePlugin] = useUpdatePluginMutation();

	const currentConfig = useMemo(() => {
		const cfg = (selectedPlugin?.config as KafkaConfigSchema) ?? {};
		return {
			...cfg,
			enabled: selectedPlugin?.enabled ?? false,
		};
	}, [selectedPlugin]);

	const handleSave = (config: KafkaFormSchema): Promise<void> => {
		return new Promise((resolve, reject) => {
			const kafkaCfg = config.kafka_config;

			const payload: Record<string, unknown> = {
				brokers: kafkaCfg.brokers.filter((b) => b.trim() !== ""),
				topic: kafkaCfg.topic,
				flush_frequency_ms: kafkaCfg.flush_frequency_ms,
				max_buffer_size: kafkaCfg.max_buffer_size,
			};

			if (kafkaCfg.client_id?.trim()) {
				payload.client_id = kafkaCfg.client_id.trim();
			}

			if (kafkaCfg.sasl_enabled && kafkaCfg.sasl) {
				payload.sasl = {
					mechanism: kafkaCfg.sasl.mechanism,
					username: toOptionalEnvVarPayload(kafkaCfg.sasl.username),
					password: toOptionalEnvVarPayload(kafkaCfg.sasl.password),
				};
			}

			if (kafkaCfg.tls_enabled && kafkaCfg.tls) {
				payload.tls = {
					enabled: true,
					skip_verify: kafkaCfg.tls.skip_verify,
					ca_cert: kafkaCfg.tls.ca_cert || undefined,
				};
			}

			updatePlugin({
				name: "kafka",
				data: {
					enabled: config.enabled,
					config: payload,
				},
			})
				.unwrap()
				.then(() => {
					toast.success("Kafka configuration saved successfully");
					resolve();
				})
				.catch((err) => {
					toast.error("Failed to save Kafka configuration", {
						description: getErrorMessage(err),
					});
					reject(err);
				});
		});
	};

	return (
		<div className="flex w-full flex-col gap-4">
			<div className="flex w-full flex-col gap-3">
				<KafkaFormFragment currentConfig={currentConfig} onSave={handleSave} onDelete={onDelete} isDeleting={isDeleting} />
			</div>
		</div>
	);
}